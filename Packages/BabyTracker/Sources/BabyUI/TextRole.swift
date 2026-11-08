import SwiftUI

/// The app's text hierarchy. Every role uses a Dynamic Type text style, so all
/// of them scale.
public enum TextRole: CaseIterable, Sendable {
    case screenTitle, sheetTitle, cardTitle, sectionLabel, body, value, caption
}

extension TextRole {
    /// The single table the modifier builds its font from. Every role names a
    /// Dynamic Type text style, so a role cannot silently become fixed-size.
    var textStyle: Font.TextStyle {
        switch self {
        case .screenTitle: .title
        case .sheetTitle, .cardTitle: .headline
        case .sectionLabel, .caption: .footnote
        case .body, .value: .body
        }
    }

    /// `nil` keeps the text style's own weight.
    var weight: Font.Weight? {
        switch self {
        case .screenTitle: .bold
        case .sectionLabel: .semibold
        case .value: .semibold
        case .sheetTitle, .cardTitle, .body, .caption: nil
        }
    }

    var isRounded: Bool { self == .screenTitle }
    var isMonospacedDigit: Bool { self == .value }
    var isSecondary: Bool { self == .sectionLabel || self == .caption }
}

private struct TextRoleModifier: ViewModifier {
    let role: TextRole

    func body(content: Content) -> some View {
        content
            .font(.system(role.textStyle, design: role.isRounded ? .rounded : .default, weight: role.weight))
            .monospacedDigit(role.isMonospacedDigit)
            .foregroundStyle(role.isSecondary ? Palette.secondaryText : Palette.primaryText)
    }
}

private extension View {
    @ViewBuilder
    func monospacedDigit(_ on: Bool) -> some View {
        if on { monospacedDigit() } else { self }
    }
}

public extension View {
    func textRole(_ role: TextRole) -> some View {
        modifier(TextRoleModifier(role: role))
    }
}
