import Foundation
import Observation
import SwiftUI
import os
import BabyCore

/// One failure at a time, surfaced to the user and logged for later.
///
/// Feature modules reach this through the SwiftUI environment rather than
/// importing the app target, so a sheet in `LoggingFeature` can report without
/// knowing `AppEnvironment` exists.
@MainActor
@Observable
public final class ErrorReporter {
    public private(set) var current: String?

    /// `nonisolated` so the `@Entry` default below — which the SwiftUI macro
    /// materialises in a nonisolated context — can construct one. The
    /// initialiser touches no isolated state.
    public nonisolated init() {}

    /// `message` is what the user reads; `error` is what the log records.
    /// Only the localized description is logged — never entry contents.
    public func report(
        _ message: String,
        error: any Error,
        category: Logger = AppLog.ui
    ) {
        current = message
        // `message` is our own copy, so it stays `.public`. The error's
        // description is not: a SwiftData validation failure can name the
        // entity and the property it rejected, which is entry content by
        // another route. `.private` keeps it readable with a device attached
        // and redacts it in collected logs pasted into bug reports.
        category.error("\(message, privacy: .public): \(error.localizedDescription, privacy: .private)")
    }

    /// Call on a successful write. A banner that outlives the problem is worse
    /// than no banner: a parent who believes it re-logs and creates a
    /// duplicate, which is the failure this release exists to stop.
    public func clear() {
        current = nil
    }

    /// The user tapped the X.
    public func dismiss() {
        current = nil
    }
}

public extension EnvironmentValues {
    @Entry var errorReporter: ErrorReporter = ErrorReporter()
}
