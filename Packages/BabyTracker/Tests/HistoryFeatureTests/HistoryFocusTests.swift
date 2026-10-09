import Foundation
import Testing
import BabyCore
import BabyStore
import BabyUI
@testable import HistoryFeature

@MainActor @Suite struct HistoryFocusTests {
    @Test func aFilterFocusShowsOnlyThatTrackerAndOpensNothing() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let model = HistoryViewModel(store: store, babyID: baby.id)
        #expect(model.apply(HistoryFocus(filter: [.growth])) == nil)
        #expect(model.filter == [.growth])
        #expect(model.isFiltered)
    }

    @Test func anEntryFocusClearsTheFilterAndNamesTheEntry() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let model = HistoryViewModel(store: store, babyID: baby.id)
        model.filter = [.feed]
        let id = UUID()
        #expect(model.apply(HistoryFocus(entryID: id)) == id)
        #expect(!model.isFiltered)
    }

    /// The app clears its focus once History reports it handled; neither
    /// that nil nor the same focus seen again may touch the filter the
    /// parent has set since.
    @Test func aFocusIsConsumedOnceAndNilAsksNothing() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let model = HistoryViewModel(store: store, babyID: baby.id)
        #expect(model.consume(nil) == nil)
        #expect(!model.isFiltered)
        let focus = HistoryFocus(filter: [.growth])
        #expect(model.consume(focus) == focus)
        #expect(model.filter == [.growth])
        model.filter = Set(HistoryViewModel.trackers)
        #expect(model.consume(focus) == nil)
        #expect(model.consume(nil) == nil)
        #expect(!model.isFiltered)
    }
}
