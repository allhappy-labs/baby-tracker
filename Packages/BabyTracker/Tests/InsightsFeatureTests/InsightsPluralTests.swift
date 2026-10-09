import Foundation
import Testing
@testable import InsightsFeature

/// The catalog's count forms, as each language picks them.
@Suite struct InsightsPluralTests {
    private func format(_ key: String, _ language: String, _ count: Int) throws -> String {
        let path = try #require(Bundle.module.path(forResource: language, ofType: "lproj"))
        let bundle = try #require(Bundle(path: path))
        return String(format: NSLocalizedString(key, bundle: bundle, comment: ""),
                      locale: Locale(identifier: language), count)
    }

    @Test func ukrainianCountsTakeTheirOwnForm() throws {
        let counts = [1, 2, 5, 11, 21, 22]
        #expect(try counts.map { try format("%lld feeds / day", "uk", $0) } == [
            "1 годування / день", "2 годування / день", "5 годувань / день",
            "11 годувань / день", "21 годування / день", "22 годування / день",
        ])
        #expect(try counts.map { try format("%lld diapers / day", "uk", $0) } == [
            "1 підгузок / день", "2 підгузки / день", "5 підгузків / день",
            "11 підгузків / день", "21 підгузок / день", "22 підгузки / день",
        ])
        #expect(try counts.map { try format("%lld days", "uk", $0) } == [
            "1 день", "2 дні", "5 днів", "11 днів", "21 день", "22 дні",
        ])
    }

    @Test func germanAndEnglishSingularsAreSingular() throws {
        #expect(try format("%lld feeds / day", "de", 1) == "1 Mahlzeit / Tag")
        #expect(try format("%lld feeds / day", "de", 3) == "3 Mahlzeiten / Tag")
        #expect(try format("%lld diapers per day", "en", 1) == "1 diaper per day")
    }

    /// The headline draws the number and the unit apart, so every count
    /// form must start with its number.
    @Test func everyCountUnitFormStartsWithItsNumber() throws {
        let catalog = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().appending(path: "Sources/InsightsFeature/Localizable.xcstrings")
        let json = try #require(JSONSerialization.jsonObject(with: Data(contentsOf: catalog)) as? [String: Any])
        let strings = try #require(json["strings"] as? [String: [String: Any]])
        for key in ["%lld feeds / day", "%lld diapers / day", "%lld feeds per day", "%lld diapers per day"] {
            let localizations = try #require(strings[key]?["localizations"] as? [String: [String: Any]], "\(key)")
            #expect(Set(localizations.keys) == ["en", "de", "es", "fr", "it", "uk"], "\(key)")
            for (language, localization) in localizations {
                let plural = try #require((localization["variations"] as? [String: Any])?["plural"] as? [String: [String: Any]],
                                          "\(key) \(language)")
                for (category, form) in plural {
                    let value = (form["stringUnit"] as? [String: String])?["value"] ?? ""
                    #expect(value.hasPrefix("%lld "), "\(key) \(language) \(category): \(value)")
                }
            }
        }
    }
}
