# Design System Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every surface of the app is built from one small kit of generic components in `BabyUI`, in the approved "quiet fills" look, with no behaviour change.

**Architecture:** Stage 1 adds tokens and components to `BabyUI` (no screen changes). Stages 2–6 migrate one area each onto the kit and delete the private types they replace. Each stage is shippable: tests, build, UI tests for touched screens, screenshots, commit, install on the phones, `TASKS.md` entry.

**Tech Stack:** Swift 6, SwiftUI (iOS 26), Swift Testing, XCUITest, WidgetKit/ActivityKit. Package at `Packages/BabyTracker`.

**Spec:** `docs/superpowers/specs/2026-11-07-design-system-design.md` (read it first; mockups in `mockups/2026-11-07-design-system/`).

## Global Constraints

- No behaviour change. Copy changes only: History filter "Done" → ✕ plus footer "Apply"; pump amount "Skip" becomes the secondary footer button.
- Keep every existing `accessibilityIdentifier` string (231 today). Components take/forward identifiers; never rename one.
- The sheet close button is the `xmark` glyph but keeps `accessibilityLabel` = the old text title ("Cancel"/"Back", localised): UI tests query `app.buttons["Cancel"]`.
- No colour, corner-radius or opacity literal outside `BabyUI` when a stage finishes (chart marks in Insights/Growth may set mark opacity).
- No `Form`/`List` inside a `fittedSheet` (it measures them as zero height).
- No new package dependency; no snapshot library.
- Glass only on `GlassDock` and the system tab bar.
- Dynamic Type scales everything; Ukrainian must wrap, never truncate silently.
- Tokens: `Radius` chip=capsule, control 14, button 18, card 20, sheet 28. `Spacing` xs 4, s 8, m 12, l 16, xl 20. `Surface` card = white 6%, raised = white 10%, separator = white 7%, tint soft 16% / strong 30%.
- Simulator: shared `iPhone 17 Pro` `<simulator-udid>`, every run through `sim-lock`.
- `make test` asserts `EXPECTED_TESTS` in `Makefile` (1961 today): bump it in the same commit that adds tests.
- Commits: on branch `design-system`, author from git config, trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. No push.

## Review Focus

1. **Accessibility5 Dynamic Type in a fitted sheet** — header and footer stay on screen, content scrolls. `ImageRenderer` cannot prove this (a fixed frame always renders at its size), so it is checked by accessibility5 screenshots of every migrated sheet in Task 8 Step 4 and by `AccessibilityAuditTests` in Task 8 Step 3.
2. **Ukrainian strings in footer buttons and chips** — wrap to two lines, not "…"; `UkrainianLayoutSmokeTests` run at stages 2, 3, 5.
3. **Disabled primary action** — visibly disabled *and* not tappable (today `opacity(0.5)` + `.disabled`); tested in Task 4 (`primaryButtonDisabledDims`).
4. **Swipe-to-delete in History after rows move into cards** — must still work and still confirm; `HistoryRedesignTests` at stage 4.
5. **Read-only caregiver** — sheets with no save path show no primary button and no dead footer; tested in Task 5 (`footerLayoutWithoutActionsIsEmpty`), `ReadOnlyCaregiverUITests` at stage 2.

## Verification commands (used by every task)

- Package: `cd <repo> && make test` → `OK <EXPECTED_TESTS>`.
- One suite: `cd Packages/BabyTracker && swift test --filter <SuiteName>`.
- Build: `cd <repo> && sim-lock xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build` → `BUILD SUCCEEDED`.
- UI subset: `sim-lock xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' -parallel-testing-enabled NO -only-testing:BabyTrackerUITests/<Class>` → `TEST SUCCEEDED`.
- Screenshots: launch with the UI-test fixture family (`UITestFamily.swift`) via `sim-lock`, `xcrun simctl io <simulator-udid> screenshot <path>`; review each touched screen at default and accessibility5.

## File map

New in `Packages/BabyTracker/Sources/BabyUI/`:
- `Tokens.swift` — `Radius`, `Spacing`, `Surface`.
- `TextRole.swift` — `TextRole`, `.textRole(_:)`; `Typography.swift` gains `NumeralSize.small`.
- `Card.swift` — `Card`, `CardSection` (replaces `PanelSection.swift`).
- `ListRow.swift` — `ListRow`.
- `Chip.swift` — `Chip`, `SelectableChipStyle`.
- `Buttons.swift` — `PrimaryButtonStyle`, `SecondaryButtonStyle`, `DestructiveButton`, `CircleIconButton`, `TileButtonStyle`, `.confirmDelete`. Replaces `PrimaryFillButtonStyle.swift`, `TrackerButtonStyle.swift`.
- `SheetScaffold.swift` — `SheetScaffold`, `SheetAction`, `CloseButton`; `FittedSheet.swift` loses `SheetHeader`, `SheetTitleText`, `SheetTopFill`.
- `ConfirmSheet.swift`, `ScreenScaffold.swift`, `EmptyState.swift`, `InlineError.swift`, `PartsRow.swift`.
- `KitGallery.swift` — `#Preview` only, `#if DEBUG`.
- Delete: `FeedStepConnector.swift`.

Tests in `Packages/BabyTracker/Tests/BabyUITests/`: `TokensTests.swift`, `CardTests.swift`, `ChipTests.swift`, `ButtonsTests.swift`, `SheetScaffoldTests.swift`, `ScaffoldsTests.swift`.

---

## Stage 1 — Kit

### Task 1: Tokens and text roles

**Files:** Create `BabyUI/Tokens.swift`, `BabyUI/TextRole.swift`; modify `BabyUI/Typography.swift`; test `Tests/BabyUITests/TokensTests.swift`.

**Interfaces — Produces:**
- `public enum Radius { static let control: CGFloat = 14, button: CGFloat = 18, card: CGFloat = 20, sheet: CGFloat = 28 }` (chip uses `Capsule()`; no constant).
- `public enum Spacing { static let xs: CGFloat = 4, s = 8, m = 12, l = 16, xl = 20 }`.
- `public enum Surface { static let card: Color, raised: Color, separator: Color; enum Strength { case soft, strong; var opacity: Double }; static func tint(_ color: Color, _ strength: Strength = .soft) -> Color }` — card = `.white.opacity(0.06)`, raised 0.10, separator 0.07, soft 0.16, strong 0.30.
- `public enum TextRole: CaseIterable { case screenTitle, sheetTitle, cardTitle, sectionLabel, body, value, caption }`, `extension View { func textRole(_ role: TextRole) -> some View }`. Fonts: screenTitle `.title.bold` rounded; sheetTitle `.headline`; cardTitle `.headline`; sectionLabel `.footnote.weight(.semibold)` + `Palette.secondaryText`; body `.body`; value `.body.weight(.semibold).monospacedDigit()`; caption `.footnote` + `Palette.secondaryText`. Primary roles use `Palette.primaryText`.
- `NumeralSize.small` → `.headline` rounded bold, monospaced digits.

- [ ] **Step 1: Write failing tests** `tokenValuesMatchSpec` (asserts every constant above, and `Surface.Strength.soft.opacity == 0.16`, `.strong == 0.30`), `everyTextRoleRendersAndScales` (for each `TextRole`, `Text("Ag").textRole(role)` renders taller at `.accessibility5` than `.large` with `ImageRenderer`, width 320).
- [ ] **Step 2:** `swift test --filter TokensTests` → fails (types missing).
- [ ] **Step 3:** Implement the interfaces above.
- [ ] **Step 4:** `swift test --filter TokensTests` → pass.
- [ ] **Step 5:** Bump `EXPECTED_TESTS` by 2; `make test` OK; commit `feat(ui): design tokens and text roles`.

### Task 2: Card, CardSection, ListRow

**Files:** Create `BabyUI/Card.swift`, `BabyUI/ListRow.swift`; mark `PanelSection` `@available(*, deprecated, message: "Use CardSection")` and leave it unchanged so stage 1 changes no screen (deleted in Task 12); test `CardTests.swift`.

**Interfaces — Consumes:** Task 1 tokens. **Produces:**
- `public struct Card<Content: View>: View { init(padding: CGFloat = Spacing.m, @ViewBuilder content: () -> Content) }` — `Surface.card` fill, `RoundedRectangle(cornerRadius: Radius.card, style: .continuous)`.
- `public struct CardSection<Header: View, Content: View, Footer: View>` with `init(@ViewBuilder header:, @ViewBuilder content:, @ViewBuilder footer:)` plus convenience inits without header/footer and `init(_ title: String, …)` (header = `Text(title).textRole(.sectionLabel)`). Same row behaviour as today's `PanelSection` (each direct child a row, ≥44 pt, `Spacing.l` horizontal, hairline `Surface.separator` inset `Spacing.l`, hidden child takes no row), card fill `Surface.card`, radius `Radius.card`.
- `public struct ListRow<Leading: View, Trailing: View>: View` with `enum Accessory { case none, disclosure, expand(isExpanded: Bool) }`; `init(_ title: String, subtitle: String? = nil, accessory: Accessory = .none, @ViewBuilder leading:, @ViewBuilder trailing:)`, conveniences `init(_ title:, value: String?, icon: BabyIcon? = nil, tint: Color = Palette.primaryText, accessory:)`. Disclosure = `chevron.right`; expand = `chevron.down` rotated −90° when collapsed, animated. Accessory glyph `Palette.secondaryText`, `accessibilityHidden`. Value uses `.textRole(.value)`. Row is not itself a button (wrap in `Button` at call site).

- [ ] **Step 1: Failing tests** `cardSectionHiddenRowTakesNoSpace` (a section with `if false { Text("x") }` + one row renders same height as one row), `cardSectionHeaderAddsHeight` (with title > without), `listRowExpandRotation` (`ListRow.chevronRotation(for: .expand(isExpanded: false)) == .degrees(-90)`, `true` → `.zero`; expose that static helper).
- [ ] **Step 2:** run → fail. **Step 3:** implement. **Step 4:** run → pass.
- [ ] **Step 5:** `make build` succeeds; bump count; commit `feat(ui): Card, CardSection, ListRow`.

### Task 3: Chip and selectable chip

**Files:** Create `BabyUI/Chip.swift`; test `ChipTests.swift`.

**Interfaces — Produces:**
- `public struct Chip: View { enum Size { case small, regular }; init(_ text: String, icon: BabyIcon? = nil, systemImage: String? = nil, tint: Color = Palette.secondaryText, size: Size = .regular, onRemove: (() -> Void)? = nil, removeLabel: String? = nil) }` — capsule, fill `Surface.tint(tint, .soft)`, label in `tint` (or `Palette.primaryText` when tint is `secondaryText`), small = `.caption.weight(.semibold)` 3/8 padding, regular = `.subheadline.weight(.semibold)` 6/12; trailing `xmark` button 44 pt hit area when `onRemove` set, labelled `removeLabel`. One line, `fixedSize` for times.
- `public struct SelectableChipStyle: ButtonStyle { init(tint: Color, isSelected: Bool) }`, `extension ButtonStyle where Self == SelectableChipStyle { static func selectableChip(_ tint: Color, isSelected: Bool) -> Self }`. Unselected: soft tint fill, `tint` label. Selected: `tint` fill, `Palette.ground` label, `.isSelected` accessibility trait. Min height 44, capsule, label may wrap to 2 lines.
- `static func fill(tint: Color, isSelected: Bool) -> Color` and `static func labelColor(tint:isSelected:) -> Color` exposed for tests.

- [ ] **Step 1: Failing tests** `selectableChipColours` (selected fill == tint, label == `Palette.ground`; unselected fill == `Surface.tint(tint, .soft)`, label == tint), `chipWrapsUkrainianAtAccessibility5` (a `SelectableChipStyle` button with "Грудне молоко та суміш" at width 120, accessibility5 renders taller than one line at `.large`).
- [ ] **Steps 2–4:** fail → implement → pass.
- [ ] **Step 5:** bump; commit `feat(ui): Chip and selectable chip style`.

### Task 4: Buttons, tiles, delete confirmation

**Files:** Create `BabyUI/Buttons.swift`; mark `PrimaryFillButtonStyle` and `TrackerButtonStyle` deprecated but leave them working so stage 1 changes no screen (both files deleted in Task 12). Move `deleteConfirmation` from `LoggingFeature/EditChrome.swift` into `Buttons.swift` as `confirmDelete` and leave `deleteConfirmation` in EditChrome as a one-line forward until Task 7. Test `ButtonsTests.swift`.

**Interfaces — Produces:**
- `public struct PrimaryButtonStyle: ButtonStyle { enum Size { case regular /*54*/, dock /*72*/ }; init(size: Size = .regular) }` — `Palette.primaryText` fill, `Palette.ground` label `.headline`, `Radius.button` (dock: 20), reads `@Environment(\.isEnabled)`: disabled → opacity 0.5. Pressed scale 0.97 / easeOut 0.12. `static var primary`, `static func primary(_ size:)`.
- `public struct SecondaryButtonStyle: ButtonStyle` — `Surface.raised` fill, `Palette.primaryText` label, same size/radius. `static var secondary`.
- `public struct DestructiveButton: View { init(_ title: String, identifier: String, action: @escaping () -> Void) }` — plain text button, `Palette.destructive`, `.body.weight(.semibold)`, 44 pt min, `role: .destructive`.
- `public struct CircleIconButton: View { init(systemImage: String? = nil, icon: BabyIcon? = nil, label: String, identifier: String, showsBadge: Bool = false, isProminent: Bool = false, action:) }` — 44 pt circle, `Surface.card` (prominent: `Palette.primaryText` fill, ground glyph), badge = 8 pt `Palette.feed` dot top-trailing.
- `public struct TileButtonStyle: ButtonStyle { enum Size { case large /*68*/, regular /*56*/, widget /*44*/ }; init(tint: Color, size: Size = .regular, isActive: Bool = false) }` — fill `Surface.tint(tint, isActive ? .strong : .soft)`, glyph/label `tint`, `Radius.button` (widget: `Radius.control`), pressed scale. `static func tile(_ tint:, size:, isActive:)`. `static func fillStrength(isActive: Bool) -> Surface.Strength` for tests.
- `extension View { func confirmDelete(isPresented: Binding<Bool>, title: String, deleteTitle: String, keepTitle: String, identifierPrefix: String, onDelete: @escaping () -> Void) -> some View }` — `confirmationDialog`, buttons `<prefix>.confirmDelete` / `<prefix>.keep` (same as today).

- [ ] **Step 1: Failing tests** `primaryButtonDisabledDims` (render a `.primary` button with `.disabled(true)` and without; assert `PrimaryButtonStyle.opacity(isEnabled: false) == 0.5`, `true == 1`), `tileFillStrength` (`isActive` true → `.strong`, false → `.soft`), `primaryButtonHeights` (`PrimaryButtonStyle.Size.regular.minHeight == 54`, `.dock == 72`).
- [ ] **Steps 2–4:** fail → implement → pass.
- [ ] **Step 5:** `make build` succeeds; bump; commit `feat(ui): primary/secondary/destructive/circle/tile buttons and confirmDelete`.

### Task 5: SheetScaffold and ConfirmSheet

**Files:** Create `BabyUI/SheetScaffold.swift`, `BabyUI/ConfirmSheet.swift`; modify `BabyUI/FittedSheet.swift` (delete `SheetTopFill`; keep `SheetHeader`/`SheetTitleText` as deprecated until Task 12); test `SheetScaffoldTests.swift`.

**Interfaces — Consumes:** Tasks 1–4. **Produces:**
- `public struct SheetAction { init(_ title: String, identifier: String, isEnabled: Bool = true, action: @escaping () -> Void) }`.
- `public struct SheetScaffold<Title: View, Accessory: View, Content: View>: View`:
  - `init(closeTitle: String, closeIdentifier: String? = nil, onClose: (() -> Void)?, primary: SheetAction? = nil, secondary: SheetAction? = nil, destructive: SheetAction? = nil, @ViewBuilder title:, @ViewBuilder accessory:, @ViewBuilder content:)` and `init(_ title: String, …)` using `Text(title).textRole(.sheetTitle)` + `.isHeader`.
  - Layout: header row (min 44, top `Spacing.l`, horizontal `Spacing.l`): `CloseButton` leading (`xmark` in a 36 pt `Surface.raised` circle, 44 pt hit area, `accessibilityLabel(closeTitle)`, identifier), title centred, accessory trailing. Content in `ScrollView` + `.scrollBounceBehavior(.basedOnSize)`, `VStack(spacing: Spacing.m)`, padding `Spacing.l` / `Spacing.m`. Footer pinned below the scroll view (not a safe-area inset): optional `DestructiveButton` on its own row, then `HStack(spacing: Spacing.s)` of secondary (`.secondary`) and primary (`.primary`); padding `Spacing.l` horizontal, `Spacing.s` vertical.
  - `static func footerLayout(primary: Bool, secondary: Bool, destructive: Bool) -> FooterLayout` where `public enum FooterLayout: Equatable { case none, actions(destructiveRow: Bool, secondary: Bool, primary: Bool) }`; no actions → `.none` (no footer view at all).
  - Must contain no `Form`/`List`; works in `fittedSheet` and in a full-height sheet.
  - `EntryPanel`'s `openTimeWheel` environment stays in `EntryPanel`, which becomes a thin wrapper over `SheetScaffold` in Task 7.
- `public struct ConfirmSheet: View { init(systemImage: String, tint: Color, title: String, message: String, confirm: SheetAction, isDestructive: Bool, cancel: SheetAction) }` — icon (48 pt, tint) + title (`.title3.bold`) + message (`.body`, secondary) centred; confirm uses `.primary` or, when destructive, a primary-sized button filled `Palette.destructive` with ground label; cancel `.secondary` below it. Designed for `fittedSheet`.

- [ ] **Step 1: Failing tests** `footerLayoutWithoutActionsIsEmpty` (`footerLayout(false,false,false) == .none`), `footerLayoutDeleteGetsOwnRow` (`(true,false,true) == .actions(destructiveRow: true, secondary: false, primary: true)`), `closeButtonKeepsTextLabel` (`CloseButton(title: "Cancel", …)` exposes `accessibilityLabel` "Cancel" — test the stored `title` via `CloseButton.label`).
- [ ] **Steps 2–4:** fail → implement → pass.
- [ ] **Step 5:** bump; commit `feat(ui): SheetScaffold and ConfirmSheet`.

### Task 6: Screen scaffold, empty state, inline error, parts row, gallery

**Files:** Create `BabyUI/ScreenScaffold.swift`, `EmptyState.swift`, `InlineError.swift`, `PartsRow.swift`, `KitGallery.swift`; delete `BabyUI/FeedStepConnector.swift`; test `ScaffoldsTests.swift`.

**Interfaces — Produces:**
- `public struct ScreenScaffold<Trailing: View, Content: View>: View { enum Header { case baby(name: String, avatar: BabyAvatar, switcherID: String, headingID: String, onSwitch: (() -> Void)?), title(String, identifier: String) }; init(header: Header, scrolls: Bool = true, @ViewBuilder trailing:, @ViewBuilder content:) }` — `Palette.ground.ignoresSafeArea()` background, header row = `BabySwitcherHeader` (or `Text(title).textRole(.screenTitle)`) + `HStack(spacing: Spacing.s) { trailing }`, horizontal `Spacing.l`; when `scrolls`, content in a `ScrollView` with `VStack(spacing: Spacing.m)`; when `false` (History's `List`, Ask's conversation), content fills below the header and owns its scrolling. `.toolbarBackground(.hidden)`, tab bar untouched.
- `public struct EmptyState: View { init(icon: BabyIcon? = nil, systemImage: String? = nil, title: String, message: String?) }` — centred, glyph 40 pt `Palette.secondaryText`, title `.cardTitle`, message `.caption`.
- `public struct InlineError: View { init(_ message: String, systemImage: String? = "exclamationmark.circle") }` — `.footnote`, `Palette.destructive`, wraps.
- `public struct PartsRow: View { struct Item { init(icon: BabyIcon, tint: Color, text: String) }; enum Size { case app, widget }; init(_ items: [Item], size: Size = .app, earlierBadge: String? = nil, earlierLabel: String? = nil) }` — `ViewThatFits(.horizontal)` dropping oldest items first (widget behaviour) for both sizes; app: icon 16, `.footnote.weight(.semibold)` primary text, spacing 12; widget: icon 16, `.caption.weight(.semibold)` secondary text, spacing 10; each time `lineLimit(1).fixedSize()`.
- `KitGallery`: `#if DEBUG` `#Preview("Kit")` showing every component and state from Tasks 1–6 on `Palette.ground`, and `#Preview("Kit – AX5")` at accessibility5.

- [ ] **Step 1: Failing tests** `partsRowDropsOldestFirst` (`PartsRow.visibleSlices(count: 3)` returns `[0..<3, 1..<3, 2..<3]` — expose that helper), `emptyStateScales` (taller at accessibility5), `screenScaffoldTitleHeaderHasIdentifier` (render compiles with `.title("Settings", identifier: "settings.heading")` — assert `ScreenScaffold.Header.title(...).identifier == "settings.heading"`).
- [ ] **Steps 2–4:** fail → implement → pass.
- [ ] **Step 5:** Open `KitGallery` previews in Xcode or render each with `ImageRenderer` to PNGs in the scratchpad and review against the mockups (quiet fills, no outlines). `make test`, `make build` OK.
- [ ] **Step 6:** Commit `feat(ui): ScreenScaffold, EmptyState, InlineError, PartsRow, kit gallery`; add a `TASKS.md` section "Design system (2026-11-07)" with stage 1 done and counts.

---

## Stage 2 — Sheets

Migration rule for every sheet in this stage: replace `SheetHeader` + `EditFooter`/`editFooter`/hand-built confirm button with `SheetScaffold`; `PanelSection` → `CardSection`; `.tracker(...)` chips → `.selectableChip`; `.primaryFill` → `.primary`; `.red` → `InlineError` or `Palette.destructive`; remove `.padding(.horizontal, -20)` and per-sheet `20` insets; keep every identifier (`<prefix>.save` on primary, `<prefix>.delete` on destructive, cancel identifiers on `closeIdentifier`).

### Task 7: EntryPanel and EditChrome on the kit

**Files:** Modify `LoggingFeature/EntryPanel.swift` (`EntryPanel` body → `SheetScaffold`, keeping its public init unchanged and the `openTimeWheel` environment; `DetailsSection` header → `ListRow(accessory: .expand)`; `DetailFieldRow` stays, inside `CardSection`), `LoggingFeature/EditChrome.swift` (`EditFooter` → builds `SheetAction`s; delete private `DeleteConfirmation`, forward to `confirmDelete`; `EditTimeRow` pills → `Chip`/`Radius.control`).

- [ ] **Step 1:** Run `swift test --filter LoggingFeatureTests` → record baseline pass.
- [ ] **Step 2:** Migrate; the Delete button moves from beside Save to its own row above it (spec).
- [ ] **Step 3:** `swift test --filter LoggingFeatureTests` pass; `make build` OK.
- [ ] **Step 4:** UI: `LoggingFlowTests`, `EditFlowTests` → pass.
- [ ] **Step 5:** Commit `refactor(logging): entry panel on SheetScaffold`.

### Task 8: Logging sheets

**Files:** `LoggingFeature/` `BottleSheet.swift`, `BottleAmountSection.swift`, `PumpPanel.swift` (delete `PumpConfirmButton`), `PumpAmountSheet.swift` (Skip → `secondary`), `DiaperSheet.swift`, `MedsSheet.swift`, `MedChipsRow.swift`, `SolidsSheet.swift`, `MeasurementSheet.swift`, `ImmunizationSheet.swift`, `SupplementSheet.swift`, `MoreSheet.swift` (tool rows → `ListRow(.disclosure)`, tiles → `.tile`, delete `PressDimStyle`), `PastEntrySheet.swift` (delete `FilledTileStyle`, use `.tile`), `PartEditSheet.swift`, `JoinRow.swift`, `LogLinkPanel.swift`, `TimeSpanSection.swift`, `SessionSheet.swift` (full height; header + footer from `SheetScaffold`, its `List` stays because it's not fitted).

- [ ] **Step 1:** Migrate per the stage rule, two or three files per commit, `swift test --filter LoggingFeatureTests` + `make build` before each commit.
- [ ] **Step 2:** `grep -rnE "\.red\b|opacity\(0\.[0-9]+\)|cornerRadius: [0-9]|padding\(\.horizontal, -" Sources/LoggingFeature` → no matches.
- [ ] **Step 3:** UI: `LoggingFlowTests`, `EditFlowTests`, `PumpingFlowTests`, `CompositeFeedFlowTests`, `GrowthFlowUITests`, `ReadOnlyCaregiverUITests`, `UkrainianLayoutSmokeTests`, `AccessibilityAuditTests` → pass.
- [ ] **Step 4:** Screenshots: Bottle, Pump, Pump amount, Diaper, Meds, More, Past entry, an edit panel, Session — default and accessibility5. Compare to `mockups/2026-11-07-design-system/sheet-anatomy.html` option A.

### Task 9: Home and History sheets

**Files:** `HomeFeature/RunningTimerEditSheet.swift` (Save from header `.glassProminent` → footer primary; hand-made header → `CardSection` title; error label → `InlineError`), `HistoryFeature/HistoryView.swift` filter sheet (Done → ✕, footer primary "Apply" with identifier = old Done identifier; toggles → `.selectableChip`), `HistoryFeature/HistoryExportSheet.swift` (`NavigationStack`+`Form` → `SheetScaffold` + `CardSection`).

- [ ] **Step 1:** Add the localised "Apply" string (en + uk) in `HistoryFeature/Localizable.xcstrings`.
- [ ] **Step 2:** Migrate; `swift test --filter "HomeFeatureTests|HistoryFeatureTests"`; `make build`.
- [ ] **Step 3:** UI: `RunningTimerLifecycleTests`, `HistoryRedesignTests` → pass (update a test only where it tapped "Done" by label; switch it to the identifier).
- [ ] **Step 4:** `make test` OK; commit; install on the phones (signed Debug, in place) and ask the owner to check stage 2; record in `TASKS.md`.

---

## Stage 3 — Home

### Task 10: Home on the kit

**Files:** `HomeFeature/HomeDock.swift` (delete `DockTileStyle`; L/R → `.tile(Palette.feed, size: .large, isActive: suggested)`; small tiles `.tile(..., size: .regular)`; Stop/Wake → `.primary(.dock)`), `HomeView.swift` (radial gradient → `ScreenScaffold(.baby…)`; sync card → `Card`; `.borderedProminent` → `.primary`), `TodayStripView.swift` (→ `Card`, chevron → `ListRow` accessory or the same chevron glyph styled by `ListRow`), `PumpStripView.swift` (→ `Card`, Stop capsule → `Chip` with action), `HeroView.swift` (since-pill → `Chip(size: .small)`; delete local `PartsRow`, map `PartSummary` → `PartsRow.Item`), `StatusLineView.swift` (`SleepChipView` → `Chip`), `JoinChipView.swift` (→ `Chip(onRemove:)`).

The suggested side today is a 2 pt outline; with quiet fills it becomes `isActive: true` (strong fill). Keep its accessibility hint.

- [ ] **Step 1:** Migrate; `swift test --filter HomeFeatureTests`; `make build`.
- [ ] **Step 2:** `grep -rnE "\.red\b|Color\(red|opacity\(0\.[0-9]+\)|cornerRadius: [0-9]" Sources/HomeFeature` → no matches.
- [ ] **Step 3:** UI: `RunningTimerLifecycleTests`, `CompositeFeedFlowTests`, `HomeSyncRecoveryUITests`, `PumpingFlowTests`, `UkrainianLayoutSmokeTests` → pass.
- [ ] **Step 4:** Screenshots idle / running feed / pump running / offline card, default + accessibility5. If the flat ground reads dull, add a single faint `RadialGradient` from `Surface.tint(Palette.sleep, .soft)` behind the hero only, inside `HomeView` (as a `ScreenScaffold` background overlay), and note it in `TASKS.md`.
- [ ] **Step 5:** `make test`; commit; install on phones; owner check; `TASKS.md`.

---

## Stage 4 — History, Insights, Ask

### Task 11: History, Insights, Growth, Ask

**Files:**
- `HistoryFeature/HistoryView.swift`: `ScreenScaffold(.baby…, scrolls: false)` around the existing `List`; each day section rendered as a card via `.listRowBackground(Surface.card)` + `.listSectionSpacing(Spacing.m)` + rounded section (`.listStyle(.insetGrouped)`, `.scrollContentBackground(.hidden)`) so swipe actions keep working; `HistoryChipView` → `Chip`; showing-chip → `Chip(onRemove:)`; export/filter → `CircleIconButton` (badge when a filter is active); `ContentUnavailableView` → `EmptyState`; dialogs → `.confirmDelete`.
- `InsightsFeature/InsightsView.swift` (`ScreenScaffold`, inset 12 → `Spacing.l`), `InsightSectionCard.swift`, `ImmunizationsCard.swift` (→ `Card` + `ListRow(.disclosure)`), `ImmunizationsScreen.swift` (`ScreenScaffold(.title…)` or keep nav title, `Palette.ground` background), `GrowthFeature/GrowthSectionView.swift` (→ `Card`, metric chips `.selectableChip`, error → `InlineError`), `GrowthReferenceInfoView.swift` (title → `TextRole`).
- `AskFeature/AskView.swift`: `ScreenScaffold(.baby…, scrolls: false, trailing: CircleIconButton(new chat))`; remove `accentColor`, `.secondary/.tertiary`, `.thinMaterial`, `.red`, `.white` in favour of `Palette`/`Surface`; user bubble = `Surface.tint(Palette.sleep, .soft)`, assistant icon in a `Surface.card` circle; sources → `Chip(systemImage: "arrow.up.right")`; send → `CircleIconButton(isProminent: true)`; empty state → `EmptyState`; `.alert` → `.confirmDelete`.

- [ ] **Step 1:** Migrate History; `swift test --filter HistoryFeatureTests`; UI `HistoryRedesignTests` (swipe delete + confirm still pass); commit.
- [ ] **Step 2:** Migrate Insights + Growth; `swift test --filter "InsightsFeatureTests|GrowthFeatureTests"`; UI `GrowthFlowUITests`; commit.
- [ ] **Step 3:** Migrate Ask; `swift test --filter AskFeatureTests`; UI `AskTabTests`; commit.
- [ ] **Step 4:** `grep -rnE "accentColor|thinMaterial|\.red\b|Color\.white|opacity\(0\.[0-9]+\)|cornerRadius: [0-9]" Sources/{HistoryFeature,InsightsFeature,GrowthFeature,AskFeature}` → only chart-mark opacity in Insights/Growth chart code.
- [ ] **Step 5:** Screenshots of all three tabs, default + accessibility5; `make test`; install; owner check; `TASKS.md`.

---

## Stage 5 — Settings and forms

### Task 12: Settings, form sheets, confirmations; remove shims

**Files:**
- `SettingsFeature/SettingsView.swift`, `FamilySettingsView.swift`, `AccountSettingsView.swift`, `MemberDetailView.swift`, `HomeButtonsView.swift`, `ServerSelectionView.swift`, `SyncIssueView.swift`, `ImportSummaryView.swift`, `ImportConflictView.swift`, `FamilyExportView.swift`, `FamilySharedSections.swift`, `InvitationCodeRows.swift`, `PendingInvitationRow.swift`: `Form` → `ScreenScaffold(.title(…))` (root) or a scrolling `VStack` of `CardSection` + `ListRow` on `Palette.ground` (pushed screens keep `NavigationStack` titles, `.toolbarBackground(.hidden)`). Inputs stay system controls inside rows. `.red`/`.bordered` destructive → `DestructiveButton`/`InlineError`.
- Create `BabyFeature/BabyFormFields.swift`: `public struct BabyFormFields: View` taking one `Binding` per field that both sheets share (name, birth date, growth reference, due date — use the property types the two sheets already bind) plus `identifierPrefix: String` — the fields shared by `BabyFeature/AddBabySheet.swift` and `SettingsFeature/BabyEditSheet.swift` (read both; pass exactly the fields they share; keep both sheets' identifiers by giving the prefix each one uses today).
- `AddBabySheet`, `BabyEditSheet`, `InviteSheet`, `JoinFamilySheet`, `InvitationScannerView`, `BabyFeature/BabySwitcherSheet.swift`, `HistoryExportSheet` (if not done) → `SheetScaffold` presented with `fittedSheet` (scanner and switcher may stay full height; same scaffold).
- `FamilyLifecycleConfirmationView.swift`, `JoinConfirmationView.swift` actions, `UnavailableInvitationSheet` (`PendingInvitationRow.swift`), account deletion in `AccountSettingsView.swift` → `ConfirmSheet`. `DisplayNameConfirmationView.swift` → `SheetScaffold`.
- Delete the deprecated types from Tasks 2, 4, 5 (`PanelSection.swift`, `PrimaryFillButtonStyle.swift`, `TrackerButtonStyle.swift`, `SheetHeader`, `SheetTitleText`, the `deleteConfirmation` forward) and fix any remaining callers.

- [ ] **Step 1:** Write failing test `babyFormFieldsRendersAllFields` in `Tests/BabyFeatureTests/BabyFormFieldsTests.swift` (renders; taller at accessibility5). Implement `BabyFormFields`; pass; migrate the two sheets; commit.
- [ ] **Step 2:** Migrate Settings root + family screens; `swift test --filter SettingsFeatureTests`; UI `SettingsFlowTests`, `CaregiversScreenUITests`; commit.
- [ ] **Step 3:** Migrate form sheets and confirmations; UI `FamilySyncOnboardingUITests`, `CaregiversScreenUITests`, `ReadOnlyCaregiverUITests`, `UkrainianLayoutSmokeTests`; commit.
- [ ] **Step 4:** Remove shims; `make build`; `grep -rn "PanelSection\|primaryFill\|\.tracker(\|SheetHeader\|SheetTitleText\|deleteConfirmation\|borderedProminent\|glassProminent" Packages Apps` → no matches.
- [ ] **Step 5:** Screenshots of Settings, Family, Account, Add baby, Invite, Join, a confirmation; `make test`; install; owner check; `TASKS.md`.

---

## Stage 6 — Widgets and Live Activity

### Task 13: Widgets, Live Activity, full suite

**Files:** `Apps/BabyTrackerWidgets/QuickLogWidget.swift` (delete `QuickLogTile`, use `.tile(..., size: .widget)`), `FeedActivityWidget.swift` (delete private `PartsRow`, `SideLabel`, `BottleLabel`; use `PartsRow(size: .widget, earlierBadge:, earlierLabel:)`, `.tile`/`Chip`), `PumpActivityWidget.swift`, `StatusWidget.swift` (tokens for any literals).

- [ ] **Step 1:** Migrate; `make build`.
- [ ] **Step 2:** `grep -rnE "opacity\(0\.[0-9]+\)|cornerRadius: [0-9]|Color\(red" Apps/BabyTrackerWidgets` → no matches.
- [ ] **Step 3:** UI: `WidgetTourTests`, `LiveActivityPushUITests`; screenshot each widget family and the Lock Screen card with 1, 4 and 9 parts (oldest dropped first, "+N" badge shown).
- [ ] **Step 4:** Full UI suite: `sim-lock make test-ui` with destination replaced by the shared UDID (or `sim-lock xcodebuild test … -only-testing:BabyTrackerUITests`) → `TEST SUCCEEDED`; `AccessibilityAuditTests` included.
- [ ] **Step 5:** Final sweep: `grep -rnE "Color\(red|\.red\b|opacity\(0\.[0-9]+\)|cornerRadius: [0-9]" Packages/BabyTracker/Sources Apps | grep -v "Sources/BabyUI/"` → only chart-mark opacity. `make test`, `make build`.
- [ ] **Step 6:** Commit; install on the phones; owner check; close the `TASKS.md` section with verification results.
