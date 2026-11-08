import Foundation
import os

/// Logging for the whole app.
///
/// **Binding rule: entry CONTENTS never appear in a log line.** Ids, kinds and
/// counts only — never volumes, never feed or sleep times, never notes, never a
/// baby's name. This is infant health data, and logs get pasted into bug reports
/// and support threads. `describe(entryKind:id:)` is the only sanctioned way to
/// refer to an entry, and it deliberately accepts no value parameters.
public enum AppLog {
    public static let subsystem = AppIdentity.bundlePrefix

    public static let store = Logger(subsystem: subsystem, category: "store")
    public static let timers = Logger(subsystem: subsystem, category: "timers")
    public static let sync = Logger(subsystem: subsystem, category: "sync")
    public static let widgets = Logger(subsystem: subsystem, category: "widgets")
    public static let ui = Logger(subsystem: subsystem, category: "ui")

    public static let allCategoryNames = ["store", "timers", "sync", "widgets", "ui"]

    /// The only sanctioned way to name an entry in a log line.
    public static func describe(entryKind: String, id: UUID) -> String {
        "\(entryKind) \(id.uuidString)"
    }
}
