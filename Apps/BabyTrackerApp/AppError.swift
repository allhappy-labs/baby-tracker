import Foundation

/// One error at a time. A newer failure replaces an older one rather than
/// stacking — the most recent thing the user tried is the one they care about.
struct AppError: Identifiable, Equatable {
    let id = UUID()
    let message: String
    let isFatal: Bool

    static func == (lhs: AppError, rhs: AppError) -> Bool { lhs.id == rhs.id }
}
