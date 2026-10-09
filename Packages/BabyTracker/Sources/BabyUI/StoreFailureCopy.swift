import Foundation

/// Copy for the screen shown when the store will not open at all.
///
/// It lives beside `ErrorBannerCopy` rather than in the app target so it is
/// covered by the package tests, and so the whole app's failure vocabulary sits
/// in one place. Same rules: factual, never evaluative, never blaming — and it
/// deliberately names no destructive recovery, because the only thing behind
/// that store file is a log nobody can reconstruct.
public enum StoreFailureCopy {
    public static let title = String(localized: "Couldn't open your saved entries", bundle: .module)
    public static let reassurance = String(localized: "Your entries are still on this device. Trying again usually helps.", bundle: .module)
    public static let retry = String(localized: "Try again", bundle: .module)
}
