import Testing
import Foundation
import BabyCore
import BabyStore
@testable import HistoryFeature

@Suite @MainActor struct SupplementHistoryTests {
    /// A med's row is its name, and follows an edit and a delete made
    /// anywhere — the Meds sheet's edit mode writes through these same store
    /// calls (its own tests live in `SupplementEditTests`).
    @Test func displaysEditsAndDeletesSupplement() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: store.now, referenceSex: .girls, expectedDueDate: nil)
        let entry = try store.logSupplement(name: "Iron", for: baby.id)
        let history = HistoryViewModel(store: store, babyID: baby.id)
        try history.load(dayCount: 2)
        // The name alone: no "Med" title word beside the icon.
        #expect(history.days.first?.rows.first?.text == "Iron")
        #expect(history.days.first?.rows.first?.tracker == .supplement)
        #expect(history.days.first?.rows.first?.target == .entry(.supplement(entry.id)))

        try store.editSupplement(entry, name: "Vitamin D", at: store.now.addingTimeInterval(-60), note: nil)
        try history.load(dayCount: 2)
        #expect(history.days.first?.rows.first?.text == "Vitamin D")

        try store.softDelete(entry)
        try history.load(dayCount: 2)
        #expect(history.days.isEmpty)
        #expect(try store.supplementNames(for: baby.id).isEmpty)
    }
}
