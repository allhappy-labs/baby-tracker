import SwiftUI

/// A shared tracker glyph, drawn from the app's own icon set rather than SF
/// Symbols.
public struct TrackerIcon: View {
    private let tracker: Tracker
    @ScaledMetric(relativeTo: .title2) private var iconSize = 22.0

    public init(_ tracker: Tracker) {
        self.tracker = tracker
    }

    /// Colour-neutral, like the SF Symbol it replaces: callers set the tint,
    /// whether that is `tracker.color` or something
    /// else (`HistoryFeature`'s filter chips, dimmed when off).
    public var body: some View {
        BabyIconView(tracker.icon, size: iconSize)
    }
}
