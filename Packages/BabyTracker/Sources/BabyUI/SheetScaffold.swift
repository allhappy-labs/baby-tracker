import SwiftUI

/// One footer button of a sheet. `isEnabled == false` disables the button
/// (the style also dims it).
public struct SheetAction {
    public let title: String
    public let identifier: String
    public let isEnabled: Bool
    public let action: () -> Void

    public init(_ title: String, identifier: String, isEnabled: Bool = true, action: @escaping () -> Void) {
        self.title = title
        self.identifier = identifier
        self.isEnabled = isEnabled
        self.action = action
    }
}

/// What a `SheetScaffold` footer shows. `.none` means no footer view at all.
public enum FooterLayout: Equatable, Sendable {
    case none
    case actions(destructiveRow: Bool, secondary: Bool, primary: Bool)
}

/// The shell every sheet shares: a header row (close ✕ leading, title
/// centred, accessory trailing), the content in a scroll view, and the
/// actions pinned in a footer under it.
///
/// Works presented with `fittedSheet` (the sheet is exactly as tall as this,
/// and the content scrolls once taller than the screen) and in a full-height
/// sheet (the scroll view takes the free height, the footer stays at the
/// bottom). No `Form` and no `List` inside: a fitted sheet measures them as
/// zero; group rows with `CardSection`.
public struct SheetScaffold<Title: View, Accessory: View, Content: View>: View {
    private let closeTitle: String
    private let closeIdentifier: String?
    private let onClose: (() -> Void)?
    private let primary: SheetAction?
    private let secondary: SheetAction?
    private let destructive: SheetAction?
    private let title: Title
    private let accessory: Accessory
    private let content: Content

    /// No `onClose` means no close button. `closeTitle` is the close button's
    /// accessibility label (it shows only the ✕): "Cancel", or "Back" where the
    /// sheet returns to the one it was picked from.
    public init(
        closeTitle: String,
        closeIdentifier: String? = nil,
        onClose: (() -> Void)?,
        primary: SheetAction? = nil,
        secondary: SheetAction? = nil,
        destructive: SheetAction? = nil,
        @ViewBuilder title: () -> Title,
        @ViewBuilder accessory: () -> Accessory,
        @ViewBuilder content: () -> Content
    ) {
        self.closeTitle = closeTitle
        self.closeIdentifier = closeIdentifier
        self.onClose = onClose
        self.primary = primary
        self.secondary = secondary
        self.destructive = destructive
        self.title = title()
        self.accessory = accessory()
        self.content = content()
    }

    public var body: some View {
        VStack(spacing: 0) {
            // Each slot in its own stack: the layout needs exactly three
            // subviews, and an empty accessory or a multi-view title would
            // otherwise give it zero or several.
            SheetHeaderLayout {
                HStack(spacing: 0) {
                    if let onClose {
                        SheetCloseButton(title: closeTitle, identifier: closeIdentifier, action: onClose)
                    }
                }
                HStack(spacing: 0) { title }
                HStack(spacing: Spacing.s) { accessory }
            }
            .frame(minHeight: 44)
            .padding(.horizontal, Spacing.l)
            .padding(.top, Spacing.l)

            ScrollView {
                VStack(spacing: Spacing.m) { content }
                    .padding(.horizontal, Spacing.l)
                    .padding(.vertical, Spacing.m)
            }
            .scrollBounceBehavior(.basedOnSize)

            // Under the content, not over it as a safe-area inset: an inset
            // reaches a freshly shown sheet late, so the buttons pop in.
            footer
        }
    }

    private var footer: some View {
        SheetFooter(primary: primary, secondary: secondary, destructive: destructive)
    }
}

/// A sheet's footer actions: the destructive action as text on its own row,
/// then the primary full width (with the secondary beside it while both fit,
/// otherwise stacked under it). Nothing at all with no actions. Public for a
/// sheet that pins the footer under content of its own rather than a
/// `SheetScaffold`'s.
public struct SheetFooter: View {
    private let primary: SheetAction?
    private let secondary: SheetAction?
    private let destructive: SheetAction?

    public init(primary: SheetAction? = nil, secondary: SheetAction? = nil, destructive: SheetAction? = nil) {
        self.primary = primary
        self.secondary = secondary
        self.destructive = destructive
    }

    public static func footerLayout(primary: Bool, secondary: Bool, destructive: Bool) -> FooterLayout {
        guard primary || secondary || destructive else { return .none }
        return .actions(destructiveRow: destructive, secondary: secondary, primary: primary)
    }

    public var body: some View {
        switch Self.footerLayout(
            primary: primary != nil, secondary: secondary != nil, destructive: destructive != nil) {
        case .none:
            EmptyView()
        case .actions:
            VStack(spacing: Spacing.s) {
                if let destructive {
                    DestructiveButton(destructive.title, identifier: destructive.identifier, action: destructive.action)
                        .disabled(!destructive.isEnabled)
                }
                if let primary, let secondary {
                    // Side by side while both labels fit on one line each;
                    // otherwise stacked, primary on top.
                    ViewThatFits(in: .horizontal) {
                        HStack(spacing: Spacing.s) {
                            SheetActionButton(secondary).buttonStyle(.secondary)
                            SheetActionButton(primary).buttonStyle(.primary)
                        }
                        VStack(spacing: Spacing.s) {
                            SheetActionButton(primary).buttonStyle(.primary)
                            SheetActionButton(secondary).buttonStyle(.secondary)
                        }
                    }
                } else if let primary {
                    SheetActionButton(primary).buttonStyle(.primary)
                } else if let secondary {
                    SheetActionButton(secondary).buttonStyle(.secondary)
                }
            }
            .padding(.horizontal, Spacing.l)
            .padding(.vertical, Spacing.s)
        }
    }
}

/// A `SheetAction` as a button; the caller sets the style.
struct SheetActionButton: View {
    private let action: SheetAction
    init(_ action: SheetAction) { self.action = action }

    var body: some View {
        Button(action: action.action) {
            Text(action.title)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
        }
        .disabled(!action.isEnabled)
        .accessibilityIdentifier(action.identifier)
    }
}

public extension SheetScaffold where Title == SheetScaffoldTitle {
    /// A text title.
    init(
        _ title: String,
        closeTitle: String,
        closeIdentifier: String? = nil,
        onClose: (() -> Void)?,
        primary: SheetAction? = nil,
        secondary: SheetAction? = nil,
        destructive: SheetAction? = nil,
        @ViewBuilder accessory: () -> Accessory,
        @ViewBuilder content: () -> Content
    ) {
        self.init(closeTitle: closeTitle, closeIdentifier: closeIdentifier, onClose: onClose,
                  primary: primary, secondary: secondary, destructive: destructive,
                  title: { SheetScaffoldTitle(title) }, accessory: accessory, content: content)
    }
}

public extension SheetScaffold where Title == SheetScaffoldTitle, Accessory == EmptyView {
    /// A text title and no accessory.
    init(
        _ title: String,
        closeTitle: String,
        closeIdentifier: String? = nil,
        onClose: (() -> Void)?,
        primary: SheetAction? = nil,
        secondary: SheetAction? = nil,
        destructive: SheetAction? = nil,
        @ViewBuilder content: () -> Content
    ) {
        self.init(title, closeTitle: closeTitle, closeIdentifier: closeIdentifier, onClose: onClose,
                  primary: primary, secondary: secondary, destructive: destructive,
                  accessory: { EmptyView() }, content: content)
    }
}

public extension SheetScaffold where Accessory == EmptyView {
    /// A custom title and no accessory.
    init(
        closeTitle: String,
        closeIdentifier: String? = nil,
        onClose: (() -> Void)?,
        primary: SheetAction? = nil,
        secondary: SheetAction? = nil,
        destructive: SheetAction? = nil,
        @ViewBuilder title: () -> Title,
        @ViewBuilder content: () -> Content
    ) {
        self.init(closeTitle: closeTitle, closeIdentifier: closeIdentifier, onClose: onClose,
                  primary: primary, secondary: secondary, destructive: destructive,
                  title: title, accessory: { EmptyView() }, content: content)
    }
}

/// A sheet's text title. Wraps rather than truncates.
public struct SheetScaffoldTitle: View {
    private let text: String
    public init(_ text: String) { self.text = text }
    public var body: some View {
        Text(text)
            .textRole(.sheetTitle)
            .multilineTextAlignment(.center)
            .fixedSize(horizontal: false, vertical: true)
            .accessibilityAddTraits(.isHeader)
    }
}

/// The sheet's close button: an ✕ in a raised circle, labelled with the old
/// text title so VoiceOver and UI tests (`app.buttons["Cancel"]`) find it.
/// It grows with Dynamic Type only up to accessibility1, so at the largest
/// sizes the title keeps the row; the large content viewer shows it instead.
public struct SheetCloseButton: View {
    let title: String
    let identifier: String?
    let action: () -> Void

    public init(title: String, identifier: String? = nil, action: @escaping () -> Void) {
        self.title = title
        self.identifier = identifier
        self.action = action
    }

    public var body: some View {
        Button(action: action) { CloseGlyph() }
            .buttonStyle(PressScaleButtonStyle())
            .dynamicTypeSize(...DynamicTypeSize.accessibility1)
            .accessibilityLabel(title)
            .accessibilityShowsLargeContentViewer {
                Image(systemName: "xmark")
                Text(title)
            }
            .modifier(OptionalIdentifier(identifier))
    }
}

/// Reads the size cap set on `SheetCloseButton`, so the scaled diameter stops there too.
private struct CloseGlyph: View {
    @ScaledMetric(relativeTo: .body) private var diameter: CGFloat = 36

    var body: some View {
        Image(systemName: "xmark")
            .font(.body.weight(.semibold))
            .foregroundStyle(Palette.primaryText)
            .frame(width: diameter, height: diameter)
            .background(Surface.raised, in: Circle())
            .frame(minWidth: 44, minHeight: 44)
            .contentShape(Rectangle())
    }
}

struct OptionalIdentifier: ViewModifier {
    let identifier: String?
    init(_ identifier: String?) { self.identifier = identifier }
    func body(content: Content) -> some View {
        if let identifier { content.accessibilityIdentifier(identifier) } else { content }
    }
}

/// Leading, title, trailing: the title is centred on the row however wide the
/// sides are, and gets the width between the wider side and its mirror, so a
/// long title wraps instead of running under the buttons. Each side is
/// offered at most a third of the row, so a text accessory cannot squeeze the
/// title away.
struct SheetHeaderLayout: Layout {
    static let spacing: CGFloat = Spacing.s

    /// The widest a side may be on a row this wide.
    static func sideCap(row: CGFloat) -> CGFloat { row / 3 }

    /// The title's width on a row with sides this wide: the wider side and
    /// its gap are kept clear on both edges; an empty side needs no gap.
    static func titleWidth(row: CGFloat, leading: CGFloat, trailing: CGFloat, spacing: CGFloat) -> CGFloat {
        let side = max(leading, trailing)
        return max(row - 2 * (side > 0 ? side + spacing : 0), 0)
    }

    private func sideProposal(_ row: CGFloat?) -> ProposedViewSize {
        ProposedViewSize(width: row.map(Self.sideCap), height: nil)
    }

    private func sideSizes(_ row: CGFloat?, _ subviews: Subviews) -> (leading: CGSize, trailing: CGSize) {
        let proposal = sideProposal(row)
        let cap = row.map(Self.sideCap) ?? .infinity
        func capped(_ size: CGSize) -> CGSize { CGSize(width: min(size.width, cap), height: size.height) }
        return (capped(subviews[0].sizeThatFits(proposal)), capped(subviews[2].sizeThatFits(proposal)))
    }

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        assert(subviews.count == 3, "SheetHeaderLayout takes exactly leading, title, trailing")
        guard subviews.count == 3 else { return .zero }
        let row = proposal.width
        let sides = sideSizes(row, subviews)
        let side = max(sides.leading.width, sides.trailing.width)
        let width = row ?? (subviews[1].sizeThatFits(.unspecified).width + (side > 0 ? 2 * (side + Self.spacing) : 0))
        let titleWidth = Self.titleWidth(row: width, leading: sides.leading.width,
                                         trailing: sides.trailing.width, spacing: Self.spacing)
        let title = subviews[1].sizeThatFits(ProposedViewSize(width: titleWidth, height: nil))
        return CGSize(width: width, height: max(sides.leading.height, title.height, sides.trailing.height))
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        assert(subviews.count == 3, "SheetHeaderLayout takes exactly leading, title, trailing")
        guard subviews.count == 3 else { return }
        let sides = sideSizes(bounds.width, subviews)
        subviews[0].place(at: CGPoint(x: bounds.minX, y: bounds.midY), anchor: .leading,
                          proposal: ProposedViewSize(width: sides.leading.width, height: nil))
        subviews[2].place(at: CGPoint(x: bounds.maxX, y: bounds.midY), anchor: .trailing,
                          proposal: ProposedViewSize(width: sides.trailing.width, height: nil))
        let titleWidth = Self.titleWidth(row: bounds.width, leading: sides.leading.width,
                                         trailing: sides.trailing.width, spacing: Self.spacing)
        subviews[1].place(at: CGPoint(x: bounds.midX, y: bounds.midY), anchor: .center,
                          proposal: ProposedViewSize(width: titleWidth, height: nil))
    }
}

#if DEBUG
#Preview("SheetScaffold") {
    SheetScaffold("Feeding", closeTitle: "Cancel", onClose: {},
                  primary: SheetAction("Save", identifier: "preview.save") {},
                  destructive: SheetAction("Delete", identifier: "preview.delete") {}) {
        CardSection("Time") {
            Text("Started")
            Text("Ended")
        }
        CardSection {
            Text("Left side")
            Text("Right side")
            Text("Bottle")
        } footer: {
            Text("Saved to the shared family log.")
        }
    }
    .background(Palette.ground)
    .preferredColorScheme(.dark)
}
#endif
