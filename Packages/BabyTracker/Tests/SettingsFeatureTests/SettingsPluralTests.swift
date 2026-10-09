import Foundation
import Testing
@testable import SettingsFeature

@Suite struct SettingsPluralTests {
    @Test func expiryDaysTakeTheirOwnForm() throws {
        let path = try #require(Bundle.module.path(forResource: "uk", ofType: "lproj"))
        let bundle = try #require(Bundle(path: path))
        let format = NSLocalizedString("Expires in %d days", bundle: bundle, comment: "")
        #expect([2, 5, 21].map { String(format: format, locale: Locale(identifier: "uk"), $0) } == [
            "Спливає через 2 дні", "Спливає через 5 днів", "Спливає через 21 день",
        ])
    }
}
