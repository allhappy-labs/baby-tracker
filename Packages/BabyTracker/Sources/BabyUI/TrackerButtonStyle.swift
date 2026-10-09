import SwiftUI

/// The app's button: thin material, a coloured stroke, a coloured label.
///
/// Replaces the flat `.borderedProminent` fills that read as plain rectangles
/// of colour with text dropped on them. On the near-black ground a solid fill
/// is a slab; a material surface with a tinted edge reads as a control, keeps
/// the tracker colour meaningful (it identifies feed/sleep/diaper rather than
/// just decorating), and sits with the glass dock instead of fighting it.
///
/// Contrast is deliberate: the label carries the colour at full strength while
/// the fill stays neutral, so the text keeps its ratio against the material
/// rather than against a saturated background.
public struct TrackerButtonStyle: ButtonStyle {
    public enum Emphasis: Sendable {
        /// The main action on the surface. Slightly stronger stroke and fill.
        case primary
        /// Everything else.
        case secondary
    }

    private let tint: Color
    private let emphasis: Emphasis
    private let cornerRadius: CGFloat

    public init(tint: Color, emphasis: Emphasis = .secondary, cornerRadius: CGFloat = 22) {
        self.tint = tint
        self.emphasis = emphasis
        self.cornerRadius = cornerRadius
    }

    public func makeBody(configuration: Configuration) -> some View {
        let shape = RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
        return configuration.label
            .font(.body.weight(.semibold))
            .foregroundStyle(tint)
            // Padding first, then the 44pt floor — in the other order the two
            // ADD, and a one-word control such as a range chip came out 68pt
            // tall for no reason anyone chose.
            .padding(.vertical, 12)
            .padding(.horizontal, 14)
            .frame(maxWidth: .infinity, minHeight: 44)
            .background {
                shape
                    .fill(.ultraThinMaterial)
                    // Kept low: amber at 0.18 over the near-black ground goes
                    // muddy brown. The stroke carries the identity, the fill
                    // only lifts the surface off the background.
                    .overlay(shape.fill(tint.opacity(emphasis == .primary ? 0.10 : 0.06)))
            }
            .overlay {
                shape.strokeBorder(tint.opacity(emphasis == .primary ? 0.85 : 0.45), lineWidth: 1)
            }
            .clipShape(shape)
            // Pressed state is a scale rather than an opacity change: dimming a
            // translucent surface at 3am is nearly invisible.
            .scaleEffect(configuration.isPressed ? 0.97 : 1)
            .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
            .contentShape(shape)
    }
}

public extension ButtonStyle where Self == TrackerButtonStyle {
    static func tracker(
        _ tint: Color,
        emphasis: TrackerButtonStyle.Emphasis = .secondary,
        cornerRadius: CGFloat = 22
    ) -> TrackerButtonStyle {
        TrackerButtonStyle(tint: tint, emphasis: emphasis, cornerRadius: cornerRadius)
    }
}
