import Foundation
import Testing
@testable import HistoryFeature

/// History's day counts grow a page at a time (7, 14, 21, …), and 21 or 22
/// days take a different Ukrainian form from 7.
@Suite struct HistoryPluralTests {
    private func format(_ key: String, _ language: String, _ count: Int) throws -> String {
        let path = try #require(Bundle.module.path(forResource: language, ofType: "lproj"))
        let bundle = try #require(Bundle(path: path))
        return String(format: NSLocalizedString(key, bundle: bundle, comment: ""),
                      locale: Locale(identifier: language), count)
    }

    @Test func dayCountsTakeTheirOwnForm() throws {
        #expect(try [7, 21, 22].map { try format("Nothing in the last %lld days", "uk", $0) } == [
            "За останні 7 днів нічого немає", "За останній 21 день нічого немає", "За останні 22 дні нічого немає",
        ])
        #expect(try format("Older entries are still saved — this screen shows the last %lld days.", "uk", 21)
                == "Старіші записи збережено. Тут показано останній 21 день.")
        #expect(HistoryCopy.nothingRecentTitle(21) == "Nothing in the last 21 days")
    }
}
