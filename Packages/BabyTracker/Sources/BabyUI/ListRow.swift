import SwiftUI

/// A row that shows something: leading content, title (+ subtitle), trailing
/// content (a value) and an optional accessory glyph. Not a button itself;
/// wrap it in `Button` or `NavigationLink` at the call site. A row whose
/// trailing side is an input control is a `LabeledRow`.
public struct ListRow<Leading: View, Trailing: View>: View {
    public enum Accessory: Equatable {
        case none
        case disclosure
        case expand(isExpanded: Bool)
    }

    /// `compact` is for a row inside a card that holds other content: a
    /// subheadline title in the secondary colour and a semibold primary value.
    public enum Density: Sendable { case regular, compact }

    private let title: String
    private let subtitle: String?
    private let accessory: Accessory
    private let density: Density
    private let leading: Leading
    private let trailing: Trailing
    fileprivate var combinesChildren = false

    public init(
        _ title: String,
        subtitle: String? = nil,
        accessory: Accessory = .none,
        density: Density = .regular,
        @ViewBuilder leading: () -> Leading,
        @ViewBuilder trailing: () -> Trailing
    ) {
        self.title = title
        self.subtitle = subtitle
        self.accessory = accessory
        self.density = density
        self.leading = leading()
        self.trailing = trailing()
    }

    static func chevronRotation(for accessory: Accessory) -> Angle {
        if case .expand(let isExpanded) = accessory, !isExpanded { return .degrees(-90) }
        return .zero
    }

    @Environment(\.dynamicTypeSize) private var typeSize

    public var body: some View {
        Group {
            if typeSize.isAccessibilitySize {
                // Title above value: side by side, both wrap by the word.
                VStack(alignment: .leading, spacing: Spacing.xs) {
                    HStack(spacing: Spacing.m) {
                        leading
                        titleBlock
                        accessoryGlyph
                    }
                    trailing
                }
            } else {
                HStack(spacing: Spacing.m) {
                    leading
                    titleBlock
                    trailing
                    accessoryGlyph
                }
            }
        }
        .contentShape(Rectangle())
        .modifier(CombineIf(combines: combinesChildren))
    }

    private var titleBlock: some View {
        VStack(alignment: .leading, spacing: 2) {
            titleText
            if let subtitle {
                Text(subtitle).textRole(.caption)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    @ViewBuilder private var titleText: some View {
        switch density {
        case .regular: Text(title).textRole(.body)
        case .compact: Text(title).font(.subheadline).foregroundStyle(Palette.secondaryText)
        }
    }

    @ViewBuilder private var accessoryGlyph: some View {
        switch accessory {
        case .none:
            EmptyView()
        case .disclosure:
            glyph("chevron.right")
        case .expand:
            glyph("chevron.down")
                .rotationEffect(Self.chevronRotation(for: accessory))
                .animation(.default, value: accessory)
        }
    }

    private func glyph(_ name: String) -> some View {
        Image(systemName: name)
            .font(.footnote.weight(.semibold))
            .foregroundStyle(Palette.secondaryText)
            .accessibilityHidden(true)
    }
}

public extension ListRow where Leading == EmptyView, Trailing == EmptyView {
    init(_ title: String, subtitle: String? = nil, accessory: Accessory = .none, density: Density = .regular) {
        self.init(title, subtitle: subtitle, accessory: accessory, density: density,
                  leading: { EmptyView() }, trailing: { EmptyView() })
    }
}

public extension ListRow where Leading == AnyView, Trailing == AnyView {
    init(
        _ title: String,
        value: String?,
        icon: BabyIcon? = nil,
        tint: Color = Palette.primaryText,
        accessory: Accessory = .none,
        density: Density = .regular
    ) {
        self.init(
            title, accessory: accessory, density: density,
            leading: {
                icon.map { AnyView(BabyIconView($0).foregroundStyle(tint)) } ?? AnyView(EmptyView())
            },
            trailing: {
                // No value, no view: an empty text would still take a spacing slot.
                value.map { text in
                    switch density {
                    case .regular: AnyView(Text(text).textRole(.value).foregroundStyle(Palette.secondaryText))
                    case .compact: AnyView(Text(text).font(.subheadline.weight(.semibold)).monospacedDigit()
                        .foregroundStyle(Palette.primaryText))
                    }
                }
                    ?? AnyView(EmptyView())
            }
        )
        // A title and a text value read as one element ("End, Running").
        combinesChildren = true
    }
}

/// A label with an input control beside it (a picker, a field, a stepper);
/// at the accessibility sizes the label sits above it, leading-aligned, so
/// neither breaks mid-word. A row that only shows a value is a
/// `ListRow(_:value:)`.
public struct LabeledRow<Value: View>: View {
    private let title: String
    private let value: Value
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(_ title: String, @ViewBuilder value: () -> Value) {
        self.title = title
        self.value = value()
    }

    public var body: some View {
        layout
    }

    @ViewBuilder private var layout: some View {
        if typeSize.isAccessibilitySize {
            VStack(alignment: .leading, spacing: Spacing.xs) {
                Text(title).accessibilityHidden(true)
                value
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        } else {
            HStack(spacing: Spacing.s) {
                Text(title).accessibilityHidden(true)
                Spacer(minLength: Spacing.s)
                value
            }
        }
    }
}

private struct CombineIf: ViewModifier {
    let combines: Bool
    func body(content: Content) -> some View {
        if combines { content.accessibilityElement(children: .combine) } else { content }
    }
}
