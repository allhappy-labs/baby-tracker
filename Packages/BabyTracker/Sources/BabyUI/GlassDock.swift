import SwiftUI

/// The one place custom Liquid Glass appears. Anchored outside the scroll view
/// so it never collides with toolbar or tab-bar glass — glass cannot sample
/// glass, and overlapping produces artifacts.
public struct GlassDock<Content: View>: View {
    private let content: Content

    public init(@ViewBuilder content: () -> Content) {
        self.content = content()
    }

    public var body: some View {
        GlassEffectContainer {
            content
                .padding(9)
                .glassEffect(.regular, in: .rect(cornerRadius: 30))
        }
        .padding(.horizontal, 16)
    }
}
