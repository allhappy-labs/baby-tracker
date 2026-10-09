import Foundation
import Testing
import BabyCore
import BabyStore
@testable import BabyFeature

struct GrowthCreationTests {
    @MainActor @Test func failedCreationDoesNotDismiss() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(Date()))
        var completed = false
        #expect(throws: (any Error).self) {
            try AddBabyValidation.save(store: store, name: " ", birthDate: Date(), referenceSex: .girls,
                                       expectedDueDate: nil) { _ in completed = true }
        }
        #expect(!completed)
        #expect(try store.babies().isEmpty)
    }

    @Test func referenceIsRequiredAndWhitespaceNamesAreRejected() {
        #expect(!AddBabyValidation.canSave(name: "Mira", referenceSex: nil))
        #expect(!AddBabyValidation.canSave(name: "\n ", referenceSex: .girls))
        #expect(AddBabyValidation.canSave(name: " Mira ", referenceSex: .boys))
    }

    @Test func optionalDueDateUsesDisplayedCalendarDay() throws {
        let zone = TimeZone(secondsFromGMT: 7200)!
        let instant = ISO8601DateFormatter().date(from: "2026-09-29T23:30:00Z")!
        #expect(try AddBabyValidation.dueDate(enabled: false, date: instant, timeZone: zone) == nil)
        #expect(try AddBabyValidation.dueDate(enabled: true, date: instant, timeZone: zone)?.iso8601 == "2026-09-30")
    }
}
