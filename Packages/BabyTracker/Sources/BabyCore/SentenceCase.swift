import Foundation

public extension String {
    /// Capitalises the first character and leaves the rest alone.
    ///
    /// For free text the user typed as a phrase — a comma-joined food list,
    /// say. `.capitalized` is the wrong tool there: it title-cases every word,
    /// so "sweet potato, apple" becomes "Sweet Potato, Apple" and reads as a
    /// list of proper nouns. Lives in BabyCore because both `HomeViewModel`
    /// and `HistoryViewModel` render the same list.
    var sentenceCased: String {
        isEmpty ? self : prefix(1).uppercased() + dropFirst()
    }
}
