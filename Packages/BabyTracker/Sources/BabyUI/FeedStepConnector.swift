import SwiftUI

/// The continuous marker used by expanded feeding steps on Home and History.
public struct FeedStepConnector: View {
    public let isFirst: Bool
    public let isLast: Bool

    public init(isFirst: Bool, isLast: Bool) {
        self.isFirst = isFirst
        self.isLast = isLast
    }

    public var body: some View {
        GeometryReader { geometry in
            let dotY: CGFloat = 18
            Path { path in
                path.move(to: CGPoint(x: 4.5, y: isFirst ? dotY : 0))
                path.addLine(to: CGPoint(x: 4.5, y: isLast ? dotY : geometry.size.height))
            }
            .stroke(Palette.feed.opacity(0.4), lineWidth: 1)
            Circle()
                .fill(Palette.feed)
                .frame(width: 7, height: 7)
                .position(x: 4.5, y: dotY)
        }
        .frame(width: 9)
        .accessibilityHidden(true)
    }
}
