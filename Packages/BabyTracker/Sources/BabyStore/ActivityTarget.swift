import Foundation
import BabyCore

/// Decides which baby a Stop button acts on.
///
/// There are two paths into the same intents and they want different answers:
///
/// - A **Live Activity** button belongs to one running timer, started for one
///   child. Its attributes carry that child's id. Resolving "the currently
///   selected baby" instead writes the feed to whoever happens to be selected
///   in the app, which with two babies is a silent mis-log to the wrong child.
/// - A configured **Home Screen widget** button supplies its child's ID too.
///   A missing ID is retained for legacy callers that follow app selection.
///
/// App Intent parameters cannot be `UUID`, so the id arrives as a string and
/// is parsed here — one place, rather than at each call site.
public enum ActivityTarget {

    /// - Parameters:
    ///   - activityBabyID: the id a Live Activity or configured widget supplied,
    ///     or `nil` for a legacy selected-baby path.
    ///   - babies: every baby the store knows about.
    ///   - fallback: the selected baby, used only when no id was supplied.
    /// - Returns: the baby to act on, or `nil` when there is none to act on.
    ///
    /// An id that names no known baby returns `nil` rather than falling back:
    /// the child was deleted while the activity was on screen, and acting on
    /// somebody else is the exact failure this type exists to prevent.
    public static func resolve(
        activityBabyID: String?,
        among babies: [Baby],
        fallback: Baby?
    ) -> Baby? {
        guard let activityBabyID else { return fallback }
        guard let id = UUID(uuidString: activityBabyID) else { return nil }
        return babies.first { $0.id == id }
    }
}
