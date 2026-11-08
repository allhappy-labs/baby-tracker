import SwiftUI

/// Corner radii. Chips use `Capsule()`, so there is no constant for them.
public enum Radius {
    public static let control: CGFloat = 14
    public static let button: CGFloat = 18
    public static let card: CGFloat = 20
    public static let sheet: CGFloat = 28
    /// The dock's primary button.
    public static let dock: CGFloat = 20
}

public enum Spacing {
    public static let xs: CGFloat = 4
    public static let s: CGFloat = 8
    public static let m: CGFloat = 12
    public static let l: CGFloat = 16
    public static let xl: CGFloat = 20
}

/// Translucent fills layered over the ground.
public enum Surface {
    public static let card = Color.white.opacity(0.06)
    public static let raised = Color.white.opacity(0.10)
    public static let separator = Color.white.opacity(0.07)

    public enum Strength: Sendable {
        case soft, strong

        public var opacity: Double {
            switch self {
            case .soft: 0.16
            case .strong: 0.30
            }
        }
    }

    public static func tint(_ color: Color, _ strength: Strength = .soft) -> Color {
        color.opacity(strength.opacity)
    }
}
