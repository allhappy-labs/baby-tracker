# Design system: one look, shared components (2026-11-07)

## Goal

Every surface — the five tabs, every sheet, Settings, widgets and the Live
Activity — looks like one app, built from a small set of generic components in
`BabyUI`. Behaviour does not change.

Success:
- Every screen and sheet renders through the kit below. No feature module
  defines its own card background, chip, tile style, primary button,
  sheet header/footer or delete confirmation.
- No colour, corner radius or opacity literal outside `BabyUI` (exception:
  chart marks in Insights/Growth, which use `Palette`/`Surface` tokens but may
  set mark-specific opacity).
- All 231 existing accessibility identifiers still exist and the UI suite
  passes unchanged in intent.

Mockups approved in brainstorming: `mockups/2026-11-07-design-system/`.

## Decisions (owner, 2026-11-07)

| Question | Choice |
|---|---|
| Direction | Refine "Night Shift": same palette and mood, consolidate and adjust the system |
| Component language | **Quiet fills** — no outlines; colour in soft tinted fills; selected = solid tint |
| Tab shell | **Baby header on every tab**, flat `Palette.ground` everywhere; Settings uses the same header row with a plain "Settings" title |
| Sheet anatomy | **✕ top-left, title centred, one full-width primary action in a footer**; Delete as destructive text above the footer on edit panels |
| Settings & form sheets | **Rebuilt in the app's own cards**, not a tinted system `Form` |
| Build approach | Kit first, then migrate area by area; each stage shippable and checked on the phones |

## Out of scope

New features; copy rewrites (beyond the History-filter and pump-Skip changes
below); light mode; changes to `Palette` colour values; the app icon; adding a
snapshot-testing dependency.

## Part 1 — the kit (`BabyUI`)

### Tokens

- `Radius`: `.chip` (capsule), `.control` 14, `.button` 18, `.card` 20, `.sheet` 28.
- `Spacing`: `.xs` 4, `.s` 8, `.m` 12, `.l` 16, `.xl` 20. Screen and sheet
  horizontal inset is `.l` (16) everywhere.
- `Surface`: `.card` (white 6%), `.raised` (white 10%), `.separator`
  (white 7%), and `Surface.tint(_ color, .soft | .strong)` = 16% / 30%.
- `TextRole` (a `.textRole(_:)` modifier): `.screenTitle`, `.sheetTitle`,
  `.cardTitle`, `.sectionLabel`, `.body`, `.value` (semibold, tabular
  figures), `.caption`. `numeralStyle` keeps `.hero/.display/.compact` and
  gains `.small`. All scale with Dynamic Type.

### Components

| Component | Shape of the API | Replaces |
|---|---|---|
| `Card` | `Card(padding: Spacing = .m) { … }` — `Surface.card`, `Radius.card` | Insights/Immunizations/Growth cards, TodayStrip, PumpStrip, Home sync card |
| `CardSection` | `CardSection(header:, footer:) { rows }` — rows separated by hairlines, ≥44 pt | `PanelSection`; hand-made headers in `RunningTimerEditSheet`, `MoreSheet` |
| `ListRow` | title, optional leading glyph (`BabyIcon`/SF Symbol + tint), optional value/trailing view, `accessory: .none / .disclosure / .expand(isExpanded)` | More tool rows, Immunizations/Growth "All…", `DetailsSection`/`DetailFieldRow`, TodayStrip chevron |
| `Chip` | icon?, text, tint, `size: .small/.regular`, optional trailing ✕ action | History chips, `SleepChipView`, since-pill, `JoinChipView`, showing-chip, Ask source chip, `EditChrome` tag, pump Stop capsule |
| `.selectableChip(tint:isSelected:)` | button style: soft tint fill, selected = solid tint with `Palette.ground` label | diaper/meds/bottle preset/immunization chips, Growth metric chips, History filter toggles |
| `TileButtonStyle` | tint, `size: .large/.regular/.widget`, `isActive`; soft tint fill, active = strong tint | `DockTileStyle`, `FilledTileStyle`, More tiles + `PressDimStyle`, `QuickLogTile`, widget `SideLabel`/`BottleLabel` |
| `CircleIconButton` | glyph, optional badge dot; 44 pt hit area, `Surface.card` fill | History export/filter, Ask send, Join ✕ |
| `PrimaryButton` | label, `isEnabled`; light fill (today's `primaryFill`), `Radius.button`, 54 pt tall; the dock keeps its 72 pt via a size parameter | `EditFooter` Save, `PumpConfirmButton`, `BottleSheet.confirmButton`, `.borderedProminent`, `.glassProminent` |
| `SecondaryButton` | soft neutral fill, same size | pump "Skip", ConfirmSheet cancel |
| `DestructiveButton` + `.confirmDelete(isPresented:title:action:)` | destructive text button; one `confirmationDialog` wrapper | `EditChrome` delete modifier, History/BabyEdit dialogs, Ask `.alert`, `.red`/`.bordered` delete buttons |
| `SheetScaffold` | `SheetScaffold(title:, onClose:, primary: Action?, secondary: Action?, destructive: Action?) { content }` — grab, ✕ top-left, centred `.sheetTitle`, scrolling content at `.l` inset, footer pinned; works in `fittedSheet` and full height | `SheetHeader`/`SheetTitleText`, `EntryPanel`, `EditFooter`, `NavigationStack`+toolbar sheets, the −20 pt hacks |
| `ConfirmSheet` | icon, title, message, confirm label, `isDestructive`, cancel | `FamilyLifecycleConfirmationView`, `JoinConfirmationView` actions, `UnavailableInvitationSheet`, account deletion |
| `ScreenScaffold` | `ScreenScaffold(header: .baby(…) / .title(String), trailing: { CircleIconButtons }) { content }` — `Palette.ground`, `.l` inset, room for the tab bar | the five per-tab setups |
| `EmptyState` | icon, title, message | Ask empty state, Insights/Growth/Immunizations footnote lines, History `ContentUnavailableView` |
| `InlineError` | message (+ optional icon), `Palette.destructive` | five `.red` texts, mixed destructive texts |
| `PartsRow` | one implementation, `size: .app/.widget` | `HeroView.PartsRow`, `FeedActivityWidget.PartsRow` |

Rules:
- Identifiers move with the view, never change. How a component gets one
  (settled in the 2026-11-08 cleanup):
  - a kit view that *is* one control (`DestructiveButton`, `CircleIconButton`,
    `ActionRow`, `SheetAction`) takes a required `identifier:`;
  - a container or label (`Card`, `ListRow`, a `Chip`'s text, a system
    `Button` with a kit style) takes the caller's `.accessibilityIdentifier`;
  - a control a component draws inside itself takes its own parameter
    (`closeIdentifier`, `removeIdentifier`, `confirmDelete`'s prefix).
- Input controls stay system (`TextField`, `DatePicker`, `Toggle`, `Stepper`,
  `BottleVolumeField`) placed inside `ListRow`/`CardSection`.
- Glass stays only on `GlassDock` and the system tab bar.
- Delete `FeedStepConnector`, `SheetTopFill`, and each replaced private type
  as its last caller migrates.
- Promote `EntryPanel`/`EditChrome` behaviour that is generic into the kit;
  logging-specific bits stay in `LoggingFeature`.

## Part 2 — stages

Each stage ends with: `make test` (bump `EXPECTED_TESTS` for new tests),
`make build`, the UI tests for the touched screens via `sim-lock` on the
shared simulator, screenshots of the touched screens reviewed, a commit, an
in-place install on the phones, and a `TASKS.md` entry. Stage 1 is first;
3–6 may be reordered.

1. **Kit.** Tokens and components above; a `#Preview` gallery (not shipped)
   showing every component in every state. Unit/render tests for component
   logic (footer variants, chip selection, row accessories) using the
   `ImageRenderer` pattern from `InsightsViewRenderTests`. No screen changes.
2. **Sheets.** Bottle, Pump, Pump amount, Diaper, Meds, Solids, Measurement,
   Immunization, Supplement, More, Past entry, part edit, running-timer edit,
   entry panel, `SessionSheet` (full height, same header/footer) onto
   `SheetScaffold`; selectable chips; one delete confirmation. History filter:
   ✕ instead of "Done", footer "Apply". Pump amount: "Skip" becomes the
   secondary footer button.
3. **Home.** Dock and More tiles → `TileButtonStyle`; TodayStrip, PumpStrip,
   sync card → `Card`; hero pills → `Chip`; shared `PartsRow`. Radial
   gradient → flat ground (if it reads too flat on device, a faint glow behind
   the hero only).
4. **History, Insights, Ask.** All on `ScreenScaffold`. History days become
   `Card`s of `ListRow`s, swipe actions kept, `CircleIconButton` actions.
   Insights/Growth cards → `Card` + `CardSection`, inset 12 → 16. Ask:
   `Palette` throughout (no `accentColor`, `.thinMaterial`, `.red`), baby
   header, `Chip` sources, `CircleIconButton` send, `EmptyState`,
   `.confirmDelete`.
5. **Settings & forms.** Settings → `ScreenScaffold(.title)` + `CardSection`
   + `ListRow`. `AddBabySheet`/`BabyEditSheet` share `BabyFormFields`. Invite,
   Export, Join family, scanner, baby switcher → `SheetScaffold`. Lifecycle,
   join, invitation confirmations and account deletion → `ConfirmSheet`.
6. **Widgets & Live Activity.** Quick-log tiles and side/bottle labels →
   `TileButtonStyle(.widget)` / `Chip`; Live Activity uses `PartsRow(.widget)`.
   Full UI suite run.

## Testing

- No new dependency. Render tests for kit logic; the 19 UI-test files are the
  regression net (per-stage subset, full suite at stage 6).
- `make test` total asserted per stage; `make build` per stage.
- Screenshot review of every touched screen per stage, on the shared
  `iPhone 17 Pro` simulator via `sim-lock`.
- Accessibility: Dynamic Type at the largest size checked on Home, one sheet
  and Settings in stage 1's gallery and again at stages 2/3/5.

## Risks

- **Fitted sheets measure content:** `fittedSheet` measures a `Form` as zero
  height (why `PanelSection` exists). `SheetScaffold` must keep content
  measurable — no `Form`/`List` inside a fitted sheet.
- **Flat Home ground** may feel duller than today's gradient; fallback above.
- **Widget size limits:** `TileButtonStyle(.widget)` must fit the existing
  widget families without truncation; checked in stage 6 screenshots.
- **Localisation:** Ukrainian strings run longer; footer buttons and chips
  must wrap or shrink, never truncate silently.
