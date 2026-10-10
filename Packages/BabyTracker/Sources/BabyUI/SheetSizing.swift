import SwiftUI

/// How a sheet is sized: never by a stated height.
///
/// A panel is presented with `fittedSheet` (FittedSheet.swift) and is as tall
/// as its content. What scrolls a long, varying list — a session's parts —
/// is a system sheet at full height.
public extension View {
    /// Full height, for a system sheet that holds a long list.
    func fullHeightSheet() -> some View {
        #if os(iOS)
        presentationDetents([.large])
        #else
        self
        #endif
    }
}
