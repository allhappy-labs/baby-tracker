# Self-sizing bottom sheets on iPhone

Research date: 2026-10-10. Repository inspected at `616dcc63dd40a36050655f8dd1d61e7d137cf8ad`. Toolchain: Xcode 27.0 (27A266a), `iPhoneOS27.0.sdk`; simulator runtime iOS 26.5 on the shared iPhone 17 Pro (`<simulator-udid>`). The app targets iOS 26.0, Swift 6. This is research plus a throwaway experiment, not an implemented change.

Evidence labels used below:
- **[SDK]** read from the installed SDK (`.swiftinterface`, `.swiftdoc`, UIKit headers).
- **[Docs]** Apple developer documentation, HIG or WWDC session page.
- **[Source]** third-party source code that I read.
- **[Exp]** observed in my simulator experiment (one run per variant, iOS 26.5 simulator only, not on a device).
- **[Inference]** my reasoning, not verified.

## Question

How can an iPhone sheet size itself to its content and grow or shrink smoothly when that content changes, without any hard-coded height? The cases that matter are: Details expanding and collapsing, the keyboard, a validation line appearing, and a sheet presented over another sheet. What is the best approach for this app?

## Short answer

**Recommendation:** present content-fitted sheets through UIKit. Add a small `BabyUI` presenter, a `UIViewControllerRepresentable` that presents a `UIHostingController` as a page sheet. That sheet's only detent is `UISheetPresentationController.Detent.custom`, and its resolver returns the content's height **at the sheet's width**. Whenever that height changes, call `sheet.animateChanges { sheet.invalidateDetents() }`.

Four conditions make this work:

1. The content has an intrinsic height. That means a `VStack` or `ScrollView { VStack }` of grouped rows. `Form`/`List` and `NavigationStack` do not have one.
2. The content is pinned to the top of the sheet with a tiny custom `Layout`, so SwiftUI never centres it.
3. The resolver subtracts the hosting view's safe-area insets.
4. The keyboard is excluded from the hosting controller's safe area (`safeAreaRegions = .container`). UIKit's own sheet keyboard avoidance moves the sheet instead.

In the experiment this opened straight at the right height with no fallback-then-jump. It tracked Details opening and closing, a wrapping validation line, the keyboard and a nested sheet, with no empty bands left behind.

SwiftUI-only routes cannot do this on iPhone:
- `presentationSizing(.fitted)` is ignored for height on iPhone (verified by experiment; the docs say nothing about compact width).
- `CustomPresentationDetent.Context` cannot see the content.
- `.presentationDetents([.height(measured)])` lags by a round-trip through `@State`. In the experiment it **snapped** (without animating) when shrinking and briefly clipped the footer.

**Confidence:** medium-high that the mechanism is right. It comes from primary-source API contracts plus clean simulator recordings. It is medium for production polish until it has been run on a physical iPhone and at accessibility text sizes. The real migration cost is in the panels: `Form` and the `NavigationStack` bar must go from every fitted sheet.

## Approaches evaluated

### 1. SwiftUI `presentationSizing(.fitted)` / `PresentationSizing`

- **[SDK]** Available on iOS 18 and later. `PresentationSizing.proposedSize(for:context:)` and `PresentationSizingRoot.sizeThatFits(_:)`. `PresentationSizingContext` has **no public members**. Source: `SwiftUI.swiftmodule/arm64e-apple-ios.swiftinterface`, `PresentationSizing`, `FittedPresentationSizing`, `PresentationSizingContext`.
- **[SDK]/[Docs]** Doc text: "`.fitted` — The size of the presentation is dictated by the ideal size of the content … proposing `nil` in the horizontal and vertical dimensions." For `.form` and `.page`: "On iOS, … enforces a platform-defined floor for the vertical and horizontal dimensions." The only platform caveat in the docs is macOS user-resizing. Nothing says whether iPhone compact width respects it. Sources: [presentationSizing(_:)](https://developer.apple.com/documentation/swiftui/view/presentationsizing(_:)), [PresentationSizing](https://developer.apple.com/documentation/swiftui/presentationsizing), [fitted](https://developer.apple.com/documentation/swiftui/presentationsizing/fitted). WWDC24 introduced it as "Sheet presentation sizing is now unified and simplified across platforms … `.form` or `.page`". Source: [What's new in SwiftUI, WWDC24](https://developer.apple.com/videos/play/wwdc2024/10144/).
- **[Exp]** On iPhone, `.presentationSizing(.fitted)` and `.presentationSizing(.form.fitted(horizontal: false, vertical: true))` both presented a **full-height** sheet with the content centred inside it. The keyboard case was the same. Height was ignored in compact width.
- **Verdict:** not usable for iPhone height. The modifier is about iPad, Mac and visionOS window or form sizing.

### 2. SwiftUI `presentationDetents` with `CustomPresentationDetent`

- **[SDK]** `static func height(in context: Context) -> CGFloat?`. `PresentationDetent.Context` exposes only `maxDetentValue` and `@dynamicMemberLookup` over `EnvironmentValues`. It has **no access to the content or its size**. Sources: `PresentationDetent.Context`, `CustomPresentationDetent` in the SwiftUI interface, and [PresentationDetent.Context](https://developer.apple.com/documentation/swiftui/presentationdetent/context). No sheet-height API was added in the iOS 26 or 27 SDKs. The only new sheet API is `presentationPlacement(_:)` / `UISheetPresentationController.preferredPlacement`, for leading, centre or trailing placement on iOS 27.
- Apple engineers have offered no public solution. On the Apple Developer Forums, DTS (July 2025) answered a dynamic-height question with: "Changes to the presentationDetents are animated by default on iOS and custom animations are not yet supported … file an enhancement request" ([thread 791111](https://developer.apple.com/forums/thread/791111)). A static-var `CustomPresentationDetent` hack got no reply ([thread 750636](https://developer.apple.com/forums/thread/750636)).
- **[Exp]** I measured a `VStack` with `onGeometryChange` and fed it to `.presentationDetents([.height(h)])`, which is the common pattern. In this test the first open was clean: the measurement arrived about 50 ms after presentation started, before any visible frame at `.medium`. Growing animated. **Shrinking snapped:** the sheet top jumped to the collapsed height in one frame, and for about four frames the footer was cut off below the sheet edge while the content animated. This is the round-trip lag that the earlier `FittedSheetHeight` papered over with debouncing.
- **[Inference]** The app's earlier failures were worse because it measured a `Form`'s scroll content size and insets. Those are only known after layout, and they pass through transient values while the navigation bar and footer settle.
- **Verdict:** workable for trivial content, but it fundamentally lags by a frame and does not animate shrinking. It cannot meet "no visible jumps".

### 3. UIKit custom detent + `invalidateDetents()` + `animateChanges(_:)` (recommended)

Primary-source contract:
- **[SDK]** `UISheetPresentationController.h`:
  - `customDetentWithIdentifier:resolver:` says: "The value returned … is a height within the safe area of the sheet … returning 200 will result in a detent where the height of the sheet is 200 + safeAreaInsets.bottom when edge-attached, and just 200 when floating."
  - The same header: "If the block depends on any external inputs, call `invalidateDetents` on the sheet when the external inputs change." It also forbids setting sheet properties inside the block.
  - On `invalidateDetents`: "Call within an `animateChanges:` block to animate custom detents to their new heights."
  - The resolution context has only `containerTraitCollection` and `maximumDetentValue`. It does **not** include the width, so the resolver has to get the width elsewhere.
  - WWDC22 says the same: the value "shouldn't account for the bottom safe area inset" ([What's new in UIKit, WWDC22](https://developer.apple.com/videos/play/wwdc2022/10068/)).
- **[SDK]** `prefersPageSizing`: when `NO`, "the sheet size follows the presented view controller's `preferredContentSize`". That is the iPad form-sheet behaviour. On iPhone the height is driven by detents.
- **[Docs]** `UIHostingControllerSizingOptions.preferredContentSize` "asks for the ideal size of the content using the unspecified size proposal" ([doc](https://developer.apple.com/documentation/swiftui/uihostingcontrollersizingoptions/preferredcontentsize)). **[Exp]** Because of this, `preferredContentSize` reported the *unwrapped* ideal size (width 190 pt, height 229 pt) and ignored text that wraps at the sheet's width. **Do not use `preferredContentSize.height` as the detent.** Use `UIHostingController.sizeThatFits(in: CGSize(width: sheetWidth, height: .greatestFiniteMagnitude))` ([SDK] `UIHostingController.sizeThatFits(in:)`).
- **[Docs]** Keyboard: "medium height sheets support automatic keyboard avoidance … the sheet grows automatically to account for the keyboard, and when the keyboard dismisses, the sheet automatically collapses back down" ([Customize and resize sheets in UIKit, WWDC21](https://developer.apple.com/videos/play/wwdc2021/10063/)).

Experiment results **[Exp]**: throwaway app at `/tmp/sheetlab/Lab.swift`, recorded with `simctl io recordVideo` and analysed frame by frame from coloured markers at the top and bottom of the content.

| Case | Result |
| --- | --- |
| First presentation | Slid up directly to the fitted height. The content's top tracked monotonically from off-screen to rest. No fallback detent, no second move. The very first resolver call ran before the view had a window (width 0 → 670 pt). It was corrected about 8 ms later, before any visible frame. **Pass the presenting window's width in explicitly.** |
| Details open/close | The sheet's top edge moved smoothly in both directions, in step with the content. No band was left after collapse and nothing was clipped at rest. The only visible overlap during the animation was my test rows' own default fade-in, which is a content transition, not the sheet. |
| Validation line (wraps to 3 lines) | Fitted correctly at the sheet's width. |
| Keyboard, default safe-area regions | `sizeThatFits` **included the keyboard inset** (466 → 766 pt), so the detent tried to grow by the keyboard. With `animateChanges` it settled. With a SwiftUI-animation variant (`UIView.animate(.snappy) { invalidateDetents(); layoutIfNeeded() }`) it **oscillated 766↔772 pt indefinitely and hung the run**. |
| Keyboard, `host.safeAreaRegions = .container` | The height stayed at the content height (466 pt). UIKit raised the sheet above the keyboard with Save visible, and dropped it back on dismissal. No loop. |
| Detent value with vs without subtracting `view.safeAreaInsets` | Without subtracting, there was an extra 34 pt band under the content. After subtracting, the gap under Save was exactly the system's bottom safe area (33 pt), as the header contract describes. |
| Nested sheet (`.medium`) over it | The parent receded, then returned to its exact fitted height on dismissal. No re-measure, no jump. |
| `ScrollView { VStack }` root (`.scrollBounceBehavior(.basedOnSize)`) | Measured exactly like a bare `VStack`. Good for clamping to `maximumDetentValue` at large text sizes. |
| `NavigationStack` root, or `Form` root | `sizeThatFits` returned **0**. An invisible zero-height sheet was presented. Neither works as a fitted root. |
| Without the top-pinning layout | While the content was growing or shrinking, SwiftUI centred it in the hosting view. That briefly showed a band above the content on expand and clipped the top rows on collapse. |

Pros for this app: no `@State` round-trip, so no detent change is lost mid-presentation. No debouncing and no remembered-height cache. The system sheet look (Liquid Glass, grabber, swipe-to-dismiss, VoiceOver modality, keyboard avoidance) comes for free.

Cons and costs:
- The presentation is no longer SwiftUI `.sheet`, so the presenter must forward custom environment values. This app's `errorReporter` and `invitationLinkHandOff` are injected in `RootView` with `.environment(\.…)`. **[Inference]** A new `UIHostingController` root does not inherit a SwiftUI environment except trait-backed values.
- Panels lose `Form` styling and the system navigation bar.
- Pushes inside a fitted sheet are not supported.

### 4. Custom non-sheet overlay panel

- **[Exp]** A bottom-aligned card in a `ZStack` with `.transition(.move(edge: .bottom))` fits its content trivially, animates with it, and sat above the keyboard via SwiftUI keyboard avoidance.
- Costs (**[Inference]**, plus [`isModal`](https://developer.apple.com/documentation/swiftui/accessibilitytraits/ismodal) for VoiceOver): the app would have to rebuild dimming, interactive drag-to-dismiss with velocity, the Liquid Glass sheet material and its concentric corners, VoiceOver focus containment and escape gesture, presenting real system sheets over it, state restoration, and its interaction with tab bars and other presentations. UI tests would also no longer see a system sheet.
- **Verdict:** not worth it when (3) gets everything except the height from the system.

### 5. Third-party libraries

- **[Source]** [benedom/AutoResizingSheet](https://github.com/benedom/AutoResizingSheet) (commit `bb8654a`, July 2025). It measures with `GeometryReader` + `PreferenceKey` (`readSize`, posted through `DispatchQueue.main.async`) and either sets `.presentationDetents([.medium] → .height(h))` with a selection, or rebuilds `.custom { _ in height }` detents inside `animateChanges`. It **always opens at `.medium` first**, and its doc warns "The resizing will not work properly if your view is wrapped inside a `ScrollView`."
- **[Source]** [iradell/DynamicSheet](https://github.com/iradell/DynamicSheet) (commit `1cbd907`, December 2025, copy-paste code). It uses `onGeometryChange` → `withAnimation` → `.presentationDetents([.height(totalHeight)])`, with a full-height fallback until it has measured.
- Both use the measure-and-feed pattern (approach 2) and have its lag. Neither handles keyboard insets, `Form`, or `NavigationStack`. They add nothing over a 100-line in-house presenter.
- Blog write-ups of the same technique: [Daniel Saidi, 2026](https://danielsaidi.com/blog/2026/05/22/making-a-swiftui-sheet-automatically-size-to-fit-its-content) and [Fatbobman](https://fatbobman.com/en/snippet/how-to-make-a-swiftui-sheet-automatically-adjust-height/).

### 6. iOS 26 and Apple's own guidance

- **[Docs]** iOS 26: "partial height sheets are inset by default with a Liquid Glass background. At smaller heights, the bottom edges pull in … When transitioning to a full height sheet, the glass background gradually transitions, becoming opaque." Apple's own sample code uses fixed heights: `.presentationDetents([.height(180), .medium, .large])` ([Build a SwiftUI app with the new design, WWDC25](https://developer.apple.com/videos/play/wwdc2025/323/)). UIKit: "Sheets have an updated design in iOS 26. They adapt their appearance from smaller to larger heights" ([Build a UIKit app with the new design, WWDC25](https://developer.apple.com/videos/play/wwdc2025/284/)). Neither WWDC25 nor the iOS 27 SDK adds a content-fitting detent.
- **[Docs]** HIG: "People don't generally expect to resize sheets, so it's important to use a size that's appropriate for the content you display." It also says "Display only one sheet at a time from the main interface … close the first sheet before displaying the new one" ([HIG Sheets](https://developer.apple.com/design/human-interface-guidelines/sheets)).
- Apple's own apps: I found no primary source that documents how Maps, Find My, Wallet or Fitness size their sheets. I make no claims about them.

## Recommended design for this app

### Presenter (`BabyUI`, iOS only)

`View.fittedSheet(item:onDismiss:content:)`, with an `isPresented:` overload, mirrors `.sheet(item:)` semantics: a new `id` dismisses the current sheet and presents the new one.

The implementation is a zero-size `UIViewControllerRepresentable` whose coordinator owns a `FittedSheetController: UIHostingController<AnyView>`:
- `modalPresentationStyle = .pageSheet`.
- `sheet.detents = [.custom(identifier: .fitted) { [weak host] ctx in host?.fittedHeight(maximum: ctx.maximumDetentValue) }]`.
- `safeAreaRegions = .container`, so the keyboard is never in the measurement.
- Optional `prefersGrabberVisible = false` and `sheet.delegate = coordinator`.

Height derivation lives in one place:

```swift
let size = sizeThatFits(in: CGSize(width: width, height: .greatestFiniteMagnitude))
let h = size.height - view.safeAreaInsets.top - view.safeAreaInsets.bottom
return min(h.rounded(.up), maximum)
```

- `width` is the presenting window's bounds width in compact width. It is never `view.bounds` before the view has a window.
- Cache the value. Re-measure in `viewDidLayoutSubviews` (and on size-class or Dynamic Type changes). Only when the value changes by more than 0.5 pt, call `sheet.animateChanges { sheet.invalidateDetents() }`.
- Never call `layoutIfNeeded` from inside the re-fit, and never wrap it in a SwiftUI `Animation`. That combination looped in the experiment.

The root wrapper is a `TopFill` `Layout`:
- For a finite proposal it fills the proposal and places the content at the top.
- For a nil or huge proposal it reports the content's own height. That gives one hosting controller, synchronous measurement, and no feedback loop.

Dismissal:
- `presentationControllerDidDismiss` clears the binding and calls `onDismiss`.
- Set `isModalInPresentation` when a panel has unsaved edits, if wanted.

Environment: capture `context.environment` in `updateUIViewController` and re-apply the app's custom keys (today `errorReporter` and `invitationLinkHandOff`) to the hosted root on every update. Re-assigning `rootView` on update also keeps the content current.

### Panel shell (`EntryPanel`)

- Replace `Form` with a `ScrollView { VStack }` of grouped "cards" (rounded `secondarySystemGroupedBackground` sections with row separators) and `.scrollBounceBehavior(.basedOnSize)`. At normal sizes nothing scrolls. When content exceeds `maximumDetentValue` (accessibility sizes, long sessions), the detent clamps and the content scrolls. This also removes the special `isAccessibilitySize ? [.large]` branch.
- Replace the `NavigationStack` bar with an in-content header: icon + title, Cancel as a toolbar-style glass button. Put the Save and Delete footer as the last element of the `VStack`, not in a `safeAreaInset`. Everything that determines height is then inside the one measured view.
- Details: keep `isExpanded` state, render the rows conditionally inside the `VStack`, and toggle with `withAnimation(.snappy)`. The sheet follows through `animateChanges`. Give the inserted rows a deliberate transition (for example `.opacity.combined(with: .move(edge: .top))`) so they do not fade in under the footer.
- Validation lines: insert them the same way. Wrapping is handled because the measurement is width-aware.

### What presents what

- **Fitted sheets:**
  - Quick entries: Bottle, Diaper, More, Solids, Supplement, Measurement, part edit, timer edit.
  - Small confirmations: JoinConfirmation, PendingInvitation, FamilyLifecycleConfirmation, the History filter.
  - These replace both `fixedSheetHeight(...)` and `fullHeightSheet()`.
- **Keep system `.sheet` + `.large` + `NavigationStack`:** anything that pushes, scrolls long lists or hosts system UI: SessionSheet's part list, HistoryExport + share sheet, Settings flows, AddBaby, BabySwitcher, InvitationScanner. The HIG explicitly allows full height for content-creation sheets.
- **`LogSheetView` switching:** `onSwitchTo` currently swaps the sheet in place under one presentation with `.id(current.id)`. With the fitted presenter, swapping the root content simply re-measures, so the sheet animates to the new panel's height without dismissing.
- **Nested:** a SwiftUI `.sheet` from inside fitted content presents from the hosting controller. **[Exp]** In the UIKit-nested test the parent kept its height. **[Inference]** SwiftUI `.sheet` from the hosted content behaves the same, but this was not tested. Per the HIG, prefer replacing the sheet over stacking where possible.
- **Keyboard:** the system sheet's own avoidance raises the sheet. The panel needs no keyboard code.

## Migration steps

1. Add `FittedSheet` and `TopFill` to `BabyUI` with a debug-only preview harness. Port the `/tmp/sheetlab` checks into a UI test that compares the sheet frame with the content frame after Details toggles. Test logic, not pixel geometry, per the existing UI-test policy.
2. Add `PanelCard` / `PanelRow` / `PanelHeader` / footer primitives. Move `DetailFieldRow` and `DetailsSection` off `Form`/`DisclosureGroup`.
3. Convert `MoreSheet` (fixed 314) first. It has the smallest surface. Verify it on a physical iPhone at the default size, the largest non-accessibility size, and AX5.
4. Convert `EntryPanel` and its quick-entry callers. Switch `RootView`'s log sheet and timer editor to `.fittedSheet`.
5. Convert the small confirmations. Delete `fixedSheetHeight`, the per-sheet height constants (`JoinConfirmationView.sheetHeight`, `HistoryView` 159, `PendingInvitationRow` 320, `FamilyLifecycleConfirmationView` 340) and `fullHeightSheet` uses that became fitted.
6. Run the UI-test subset for logging and settings sheets, then manual device passes for keyboard, VoiceOver (escape gesture dismisses), swipe-to-dismiss with unsaved changes, and landscape.

## Risks and unknowns

- **Simulator only.** The experiment ran on the iOS 26.5 simulator. It did not run on a device, at accessibility text sizes, in landscape (compact height makes sheets full-screen unless `prefersEdgeAttachedInCompactHeight`), or on the iOS 27 runtime. The SDK is 27.0, but no 27 simulator runtime is installed.
- **Keyboard avoidance is system behaviour, not a contract.** It is documented only as "medium height sheets support automatic keyboard avoidance" (WWDC21). I observed it for a custom detent, but it could change.
- **Re-entrancy.** Measuring in `viewDidLayoutSubviews` caused about 60 resolver calls per scripted run, which was harmless. The caching and the 0.5 pt threshold are what keep it from looping. Keep the keyboard out of the safe area.
- **Environment forwarding** is manual. A forgotten key fails silently. Add a test or a single `forwardedEnvironment` helper.
- **Losing `Form`** means re-creating grouped styling, inline `DatePicker`/`Picker` presentation and row separators by hand. Some `Form`-only behaviours (automatic row insets for Dynamic Type, list-style focus scrolling) need checking.
- **UI tests** may need updated queries if they relied on navigation-bar buttons (Cancel now lives in content).
- **Possible future Apple API.** If Apple ships a native fitted iPhone detent in a later SDK, the presenter is a single seam to replace. I found none in the iOS 27.0 SDK.
