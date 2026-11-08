import SwiftUI

/// A read-only capsule label, optionally removable. The label is a text with
/// an optional glyph, or any view (Home's join chip holds a `PartsRow`); the
/// ✕ is its own control, a 44 pt target, identified by `removeIdentifier`.
public struct Chip: View {
    public enum Size: Sendable { case small, regular }

    private let text: String
    private let customLabel: AnyView?
    private let icon: BabyIcon?
    private let systemImage: String?
    private let tint: Color
    private let size: Size
    private let onRemove: (() -> Void)?
    private let removeLabel: String?
    private let removeIdentifier: String?
    private let monospacedDigits: Bool
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(
        _ text: String,
        icon: BabyIcon? = nil,
        systemImage: String? = nil,
        tint: Color = Palette.secondaryText,
        size: Size = .regular,
        onRemove: (() -> Void)? = nil,
        removeLabel: String? = nil,
        removeIdentifier: String? = nil,
        monospacedDigits: Bool = false
    ) {
        assert(onRemove == nil || removeLabel?.isEmpty == false, "A removable Chip needs a removeLabel for VoiceOver")
        self.text = text
        self.customLabel = nil
        self.removeIdentifier = removeIdentifier
        self.icon = icon
        self.systemImage = systemImage
        self.tint = tint
        self.size = size
        self.onRemove = onRemove
        self.removeLabel = removeLabel
        self.monospacedDigits = monospacedDigits
    }

    /// A chip whose label is a view of its own. The caller owns that label's
    /// accessibility (one element, its own label and identifier).
    public init<Label: View>(
        tint: Color = Palette.secondaryText,
        size: Size = .regular,
        onRemove: (() -> Void)? = nil,
        removeLabel: String? = nil,
        removeIdentifier: String? = nil,
        @ViewBuilder label: () -> Label
    ) {
        assert(onRemove == nil || removeLabel?.isEmpty == false, "A removable Chip needs a removeLabel for VoiceOver")
        self.text = ""
        self.customLabel = AnyView(label())
        self.icon = nil
        self.systemImage = nil
        self.tint = tint
        self.size = size
        self.onRemove = onRemove
        self.removeLabel = removeLabel
        self.removeIdentifier = removeIdentifier
        self.monospacedDigits = false
    }

    private var foreground: Color { tint == Palette.secondaryText ? Palette.primaryText : tint }
    /// `monospacedDigits` keeps a ticking time from shifting its width.
    private var font: Font {
        let base: Font = switch size {
        case .small: .caption.weight(.semibold)
        case .regular: .subheadline.weight(.semibold)
        }
        return monospacedDigits ? base.monospacedDigit() : base
    }
    private var vertical: CGFloat { size == .small ? 3 : 6 }
    private var horizontal: CGFloat { size == .small ? 8 : 12 }

    private var wraps: Bool { onRemove != nil || typeSize.isAccessibilitySize }
    /// A capsule, unless the text can wrap to several lines (a removable chip,
    /// or any chip at accessibility sizes), where a capsule would balloon
    /// into an ellipse.
    private var shape: AnyShape {
        wraps ? AnyShape(RoundedRectangle(cornerRadius: Radius.control)) : AnyShape(Capsule())
    }

    public var body: some View {
        HStack(spacing: Spacing.xs) {
            if let customLabel {
                customLabel
            } else {
                if let icon { BabyIconView(icon, size: size == .small ? 14 : 16) }
                if let systemImage { Image(systemName: systemImage) }
                Text(text)
            }
            if let onRemove {
                Button(action: onRemove) {
                    Image(systemName: "xmark").font(.caption.weight(.bold))
                        .frame(minWidth: 44, minHeight: 44)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel(removeLabel ?? "")
                .modifier(OptionalIdentifier(removeIdentifier))
                .padding(.trailing, -horizontal)
                .padding(.vertical, -(44 - 16) / 2)
            }
        }
        .font(font)
        .foregroundStyle(foreground)
        // A removable chip carries a sentence (History's "Showing: ..."), and
        // at accessibility sizes any chip can outgrow the line: both wrap by
        // the word instead of running off the screen.
        .lineLimit(wraps ? nil : 1)
        .multilineTextAlignment(.leading)
        .fixedSize(horizontal: !wraps, vertical: true)
        .padding(.vertical, vertical)
        .padding(.horizontal, horizontal)
        .background(Surface.tint(tint, .soft), in: shape)
    }
}

/// Toggle-style chip button: soft tint when off, solid tint when on.
public struct SelectableChipStyle: ButtonStyle {
    let tint: Color
    let isSelected: Bool
    @Environment(\.isEnabled) private var isEnabled

    public init(tint: Color, isSelected: Bool) {
        self.tint = tint
        self.isSelected = isSelected
    }

    static func fill(tint: Color, isSelected: Bool) -> Color {
        isSelected ? tint : Surface.tint(tint, .soft)
    }

    static func labelColor(tint: Color, isSelected: Bool) -> Color {
        isSelected ? Palette.ground : tint
    }

    public func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.subheadline.weight(.semibold))
            .foregroundStyle(Self.labelColor(tint: tint, isSelected: isSelected))
            .lineLimit(2)
            .multilineTextAlignment(.center)
            .padding(.vertical, 6)
            .padding(.horizontal, Spacing.m)
            .frame(minHeight: 44)
            .background(Self.fill(tint: tint, isSelected: isSelected), in: Capsule())
            .opacity(configuration.isPressed ? 0.7 : 1)
            .opacity(PrimaryButtonStyle.opacity(isEnabled: isEnabled))
            .contentShape(Capsule())
            .accessibilityAddTraits(isSelected ? .isSelected : [])
    }
}

public extension ButtonStyle where Self == SelectableChipStyle {
    static func selectableChip(_ tint: Color, isSelected: Bool) -> SelectableChipStyle {
        SelectableChipStyle(tint: tint, isSelected: isSelected)
    }
}
