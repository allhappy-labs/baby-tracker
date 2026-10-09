import SwiftUI

/// The one primary action on a surface that otherwise uses `TrackerButtonStyle`'s
/// tinted outlines: a light fill with dark text, for "Stop", "Wake up", "Start
/// bottle" and "Done" — the spec's own words for which control on each screen
/// gets this treatment.
///
/// Promoted from a private copy in `HomeFeature/HomeDock.swift` (Task 14) so
/// `LoggingFeature`'s sheets can draw their own primary actions the same way
/// rather than reaching for `TrackerButtonStyle`'s outline, which the spec
/// never asks for here.
public struct PrimaryFillButtonStyle: ButtonStyle {
    private let cornerRadius: CGFloat
    private let minHeight: CGFloat

    public init(cornerRadius: CGFloat = 20, minHeight: CGFloat = 72) {
        self.cornerRadius = cornerRadius
        self.minHeight = minHeight
    }

    public func makeBody(configuration: Configuration) -> some View {
        let shape = RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
        return configuration.label
            .foregroundStyle(Palette.ground)
            .padding(.horizontal, 16)
            .frame(maxWidth: .infinity, minHeight: minHeight)
            .background(shape.fill(Palette.primaryText))
            .contentShape(shape)
            // Pressed state is a scale rather than an opacity change, the
            // same reasoning as `TrackerButtonStyle`: dimming a light fill at
            // 3am is nearly invisible.
            .scaleEffect(configuration.isPressed ? 0.97 : 1)
            .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
    }
}

public extension ButtonStyle where Self == PrimaryFillButtonStyle {
    /// The dock's own size: 72 pt, 20 pt corners.
    static var primaryFill: PrimaryFillButtonStyle { PrimaryFillButtonStyle() }

    static func primaryFill(cornerRadius: CGFloat = 20, minHeight: CGFloat = 72) -> PrimaryFillButtonStyle {
        PrimaryFillButtonStyle(cornerRadius: cornerRadius, minHeight: minHeight)
    }
}
