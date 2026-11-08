import SwiftUI

/// The finished parts of a session — "L 9:02  R 4:10". Every time stays on one
/// line; when the parts do not fit the width the oldest go first, and a "+N"
/// badge stands for them.
public struct PartsRow: View {
    public struct Item {
        public let icon: BabyIcon
        public let tint: Color
        public let text: String
        public let accessibilityLabel: String?

        public init(icon: BabyIcon, tint: Color, text: String, accessibilityLabel: String? = nil) {
            self.icon = icon
            self.tint = tint
            self.text = text
            self.accessibilityLabel = accessibilityLabel
        }
    }

    public enum Size: Sendable { case app, widget }

    private let items: [Item]
    private let size: Size
    private let earlierCount: Int
    private let earlierBadge: ((Int) -> String?)?
    private let earlierLabel: ((Int) -> String)?

    /// - Parameters:
    ///   - earlierCount: parts omitted before they reached the row.
    ///   - earlierBadge: the badge text for the total omitted (those plus the
    ///     ones dropped for width); nil text hides it. Defaults to "+N".
    ///   - earlierLabel: the badge's VoiceOver label for that total.
    public init(_ items: [Item], size: Size = .app, earlierCount: Int = 0,
                earlierBadge: ((Int) -> String?)? = nil, earlierLabel: ((Int) -> String)? = nil) {
        self.items = items
        self.size = size
        self.earlierCount = earlierCount
        self.earlierBadge = earlierBadge
        self.earlierLabel = earlierLabel
    }

    /// Every part left out of the row: the omitted ones plus those dropped for width.
    static func earlierTotal(earlierCount: Int, dropped: Int) -> Int { earlierCount + dropped }

    /// The slices tried in turn, widest first, each dropping one more of the
    /// oldest items.
    static func visibleSlices(count: Int) -> [Range<Int>] {
        (0..<count).map { $0..<count }
    }

    public var body: some View {
        if !items.isEmpty {
            ViewThatFits(in: .horizontal) {
                ForEach(Array(Self.visibleSlices(count: items.count).enumerated()), id: \.offset) { dropped, slice in
                    row(Array(items[slice]), dropped: dropped)
                }
            }
        }
    }

    private var spacing: CGFloat { size == .app ? 12 : 10 }
    private var font: Font {
        size == .app ? .footnote.weight(.semibold).monospacedDigit() : .caption.weight(.semibold).monospacedDigit()
    }
    private var textColor: Color { size == .app ? Palette.primaryText : Palette.secondaryText }

    private func row(_ shown: [Item], dropped: Int) -> some View {
        HStack(spacing: spacing) {
            let total = Self.earlierTotal(earlierCount: earlierCount, dropped: dropped)
            if total > 0, let badge = earlierBadge.map({ $0(total) }) ?? "+\(total)" {
                Text(verbatim: badge)
                    .font(.caption.weight(.semibold).monospacedDigit())
                    .foregroundStyle(Palette.secondaryText)
                    .lineLimit(1)
                    .fixedSize()
                    .accessibilityLabel(earlierLabel?(total) ?? badge)
            }
            ForEach(Array(shown.enumerated()), id: \.offset) { _, item in
                HStack(spacing: size == .app ? 4 : 3) {
                    BabyIconView(item.icon, size: 16).foregroundStyle(item.tint)
                    Text(item.text)
                        .font(font)
                        .foregroundStyle(textColor)
                        // A time is never broken across lines.
                        .lineLimit(1)
                        .fixedSize()
                }
                .modifier(PartAccessibility(label: item.accessibilityLabel))
            }
        }
    }
}

private struct PartAccessibility: ViewModifier {
    let label: String?

    func body(content: Content) -> some View {
        if let label {
            content.accessibilityElement(children: .ignore).accessibilityLabel(label)
        } else {
            content
        }
    }
}
