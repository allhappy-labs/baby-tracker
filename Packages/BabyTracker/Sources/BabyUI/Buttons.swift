import SwiftUI

extension View {
    /// The kit's pressed feedback: a small scale, not a dim.
    func pressScale(_ isPressed: Bool) -> some View {
        scaleEffect(isPressed ? 0.97 : 1)
            .animation(.easeOut(duration: 0.12), value: isPressed)
    }
}

/// Plain button with the kit's pressed feedback.
struct PressScaleButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label.pressScale(configuration.isPressed)
    }
}

/// Light fill, dark label: the one primary action on a surface.
public struct PrimaryButtonStyle: ButtonStyle {
    public enum Size: Sendable {
        case regular, dock

        public var minHeight: CGFloat {
            switch self {
            case .regular: 54
            case .dock: 72
            }
        }

        var cornerRadius: CGFloat {
            switch self {
            case .regular: Radius.button
            case .dock: Radius.dock
            }
        }
    }

    @Environment(\.isEnabled) private var isEnabled
    private let size: Size

    public init(size: Size = .regular) { self.size = size }

    public static func opacity(isEnabled: Bool) -> Double { isEnabled ? 1 : 0.5 }

    public func makeBody(configuration: Configuration) -> some View {
        let shape = RoundedRectangle(cornerRadius: size.cornerRadius, style: .continuous)
        return configuration.label
            .font(.headline)
            // A label that wraps (large type) stays centred in the button.
            .multilineTextAlignment(.center)
            .foregroundStyle(Palette.ground)
            .padding(.horizontal, Spacing.l)
            .frame(maxWidth: .infinity, minHeight: size.minHeight)
            .background(shape.fill(Palette.primaryText))
            .contentShape(shape)
            .opacity(Self.opacity(isEnabled: isEnabled))
            .pressScale(configuration.isPressed)
    }
}

public extension ButtonStyle where Self == PrimaryButtonStyle {
    static var primary: PrimaryButtonStyle { PrimaryButtonStyle() }
    static func primary(_ size: PrimaryButtonStyle.Size) -> PrimaryButtonStyle { PrimaryButtonStyle(size: size) }
}

/// Raised fill, light label; same size and radius as `PrimaryButtonStyle`.
public struct SecondaryButtonStyle: ButtonStyle {
    @Environment(\.isEnabled) private var isEnabled
    private let size: PrimaryButtonStyle.Size

    public init(size: PrimaryButtonStyle.Size = .regular) { self.size = size }

    public func makeBody(configuration: Configuration) -> some View {
        let shape = RoundedRectangle(cornerRadius: size.cornerRadius, style: .continuous)
        return configuration.label
            .font(.headline)
            .multilineTextAlignment(.center)
            .foregroundStyle(Palette.primaryText)
            .padding(.horizontal, Spacing.l)
            .frame(maxWidth: .infinity, minHeight: size.minHeight)
            .background(shape.fill(Surface.raised))
            .contentShape(shape)
            .opacity(PrimaryButtonStyle.opacity(isEnabled: isEnabled))
            .pressScale(configuration.isPressed)
    }
}

public extension ButtonStyle where Self == SecondaryButtonStyle {
    static var secondary: SecondaryButtonStyle { SecondaryButtonStyle() }
}

/// Plain destructive text button.
/// Dims when disabled, like the filled styles.
public struct DestructiveButton: View {
    @Environment(\.isEnabled) private var isEnabled
    private let title: String
    private let identifier: String
    private let action: () -> Void

    public init(_ title: String, identifier: String, action: @escaping () -> Void) {
        self.title = title
        self.identifier = identifier
        self.action = action
    }

    public var body: some View {
        Button(role: .destructive, action: action) {
            Text(title)
                .font(.body.weight(.semibold))
                .foregroundStyle(Palette.destructive)
                .frame(maxWidth: .infinity, minHeight: 44)
                .contentShape(Rectangle())
        }
        .buttonStyle(PressScaleButtonStyle())
        .opacity(PrimaryButtonStyle.opacity(isEnabled: isEnabled))
        .accessibilityIdentifier(identifier)
    }
}

/// 44 pt circular icon button with an optional badge dot.
public struct CircleIconButton: View {
    private let systemImage: String?
    private let icon: BabyIcon?
    private let label: String
    private let identifier: String
    private let showsBadge: Bool
    private let isProminent: Bool
    private let action: () -> Void
    @Environment(\.isEnabled) private var isEnabled

    public init(
        systemImage: String? = nil,
        icon: BabyIcon? = nil,
        label: String,
        identifier: String,
        showsBadge: Bool = false,
        isProminent: Bool = false,
        action: @escaping () -> Void
    ) {
        assert(systemImage != nil || icon != nil, "CircleIconButton needs systemImage or icon")
        assert(!label.isEmpty, "CircleIconButton needs an accessibility label")
        self.systemImage = systemImage
        self.icon = icon
        self.label = label
        self.identifier = identifier
        self.showsBadge = showsBadge
        self.isProminent = isProminent
        self.action = action
    }

    public var body: some View {
        if systemImage == nil && icon == nil {
            EmptyView()
        } else {
            button
        }
    }

    private var button: some View {
        Button(action: action) {
            ZStack(alignment: .topTrailing) {
                glyph
                    // Capped: a 44-point circle cannot hold an accessibility-size glyph.
                    .dynamicTypeSize(...DynamicTypeSize.xxxLarge)
                    .foregroundStyle(isProminent ? Palette.ground : Palette.primaryText)
                    .frame(width: 44, height: 44)
                    .background(isProminent ? Palette.primaryText : Surface.card, in: Circle())
                if showsBadge {
                    Circle().fill(Palette.feed).frame(width: 8, height: 8)
                }
            }
            .contentShape(Circle())
        }
        .buttonStyle(PressScaleButtonStyle())
        .opacity(PrimaryButtonStyle.opacity(isEnabled: isEnabled))
        .accessibilityLabel(label)
        .accessibilityIdentifier(identifier)
    }

    @ViewBuilder private var glyph: some View {
        if let systemImage {
            Image(systemName: systemImage).font(.body.weight(.semibold))
        } else if let icon {
            BabyIconView(icon, size: 22)
        }
    }
}

/// Tinted tile: soft fill, strong when active; glyph and label in the tint.
public struct TileButtonStyle: ButtonStyle {
    public enum Size: Sendable {
        case large, regular, widget

        var minHeight: CGFloat {
            switch self {
            case .large: 68
            case .regular: 56
            case .widget: 44
            }
        }

        var cornerRadius: CGFloat { self == .widget ? Radius.control : Radius.button }

        /// A widget's four tiles share a narrow row, so they keep little side padding.
        var horizontalPadding: CGFloat { self == .widget ? Spacing.xs : Spacing.m }
    }

    @Environment(\.isEnabled) private var isEnabled
    private let tint: Color
    private let size: Size
    private let isActive: Bool
    private let isSelected: Bool

    /// `isSelected` is the kit's selection language, as on `SelectableChipStyle`:
    /// a solid tint fill with a ground-coloured label, and the selected trait.
    public init(tint: Color, size: Size = .regular, isActive: Bool = false, isSelected: Bool = false) {
        self.tint = tint
        self.size = size
        self.isActive = isActive
        self.isSelected = isSelected
    }

    static func fill(tint: Color, isActive: Bool, isSelected: Bool) -> Color {
        isSelected ? tint : Surface.tint(tint, fillStrength(isActive: isActive))
    }

    static func labelColor(tint: Color, isSelected: Bool) -> Color {
        isSelected ? Palette.ground : tint
    }

    static func fillStrength(isActive: Bool) -> Surface.Strength { isActive ? .strong : .soft }

    public func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .tileSurface(tint: tint, size: size, isActive: isActive, isSelected: isSelected)
            .opacity(PrimaryButtonStyle.opacity(isEnabled: isEnabled))
            .pressScale(configuration.isPressed)
            .accessibilityAddTraits(isSelected ? .isSelected : [])
    }
}

public extension View {
    /// The tile's colour, padding, minimum height and shape, for a label that
    /// cannot take a `ButtonStyle` (a widget's `Link`).
    func tileSurface(tint: Color, size: TileButtonStyle.Size = .regular, isActive: Bool = false,
                     isSelected: Bool = false) -> some View {
        let shape = RoundedRectangle(cornerRadius: size.cornerRadius, style: .continuous)
        return foregroundStyle(TileButtonStyle.labelColor(tint: tint, isSelected: isSelected))
            .padding(.horizontal, size.horizontalPadding)
            .padding(.vertical, Spacing.s)
            .frame(maxWidth: .infinity, minHeight: size.minHeight)
            .background(shape.fill(TileButtonStyle.fill(tint: tint, isActive: isActive, isSelected: isSelected)))
            .contentShape(shape)
    }
}

public extension ButtonStyle where Self == TileButtonStyle {
    static func tile(_ tint: Color, size: TileButtonStyle.Size = .regular, isActive: Bool = false,
                     isSelected: Bool = false) -> TileButtonStyle {
        TileButtonStyle(tint: tint, size: size, isActive: isActive, isSelected: isSelected)
    }
}

public extension View {
    /// Confirmation dialog for a destructive action. Buttons carry
    /// `<prefix>.confirmDelete` / `<prefix>.keep` so a query by label does not
    /// also match the footer's Delete.
    func confirmDelete(
        isPresented: Binding<Bool>,
        title: String,
        deleteTitle: String,
        keepTitle: String,
        identifierPrefix: String,
        message: String? = nil,
        onDelete: @escaping () -> Void
    ) -> some View {
        confirmationDialog(title, isPresented: isPresented, titleVisibility: .visible) {
            Button(deleteTitle, role: .destructive, action: onDelete)
                .accessibilityIdentifier(identifierPrefix + ".confirmDelete")
            Button(keepTitle, role: .cancel) {}
                .accessibilityIdentifier(identifierPrefix + ".keep")
        } message: {
            if let message { Text(message) }
        }
    }
}
