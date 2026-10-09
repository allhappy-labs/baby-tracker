import Testing
import Foundation
import BabyCore
import BabyStore
@testable import HistoryFeature

@MainActor
private func makeStore() throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    store.breakLimits = { .standard }
    let baby = try store.addBaby(name: "Mira", birthDate: clock.now.addingTimeInterval(-90 * 86_400),
                                 referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id, clock)
}

/// What a tap on a row asks the app to open, and how an Ask source finds its
/// entry (ruling H-R9).
@Suite @MainActor struct HistoryActionTests {
    @Test func eachRowTargetBecomesItsAction() {
        let id = UUID()
        #expect(HistoryAction(.entry(.diaper(id))) == .edit(.diaper(id)))
        #expect(HistoryAction(.entry(.feed(id))) == .edit(.feed(id)))
        #expect(HistoryAction(.session(.sleep, rootID: id)) == .session(.sleep, rootID: id))
        #expect(HistoryAction(.runningTimer(id)) == .runningTimer(id))
    }

    @Test func anAskSourceNamesItsEntryOfAnyKind() throws {
        let (store, baby, clock) = try makeStore()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        let sleep = try store.logSleep(from: clock.now.addingTimeInterval(-3600), to: clock.now, for: baby)
        let diaper = try store.logDiaper(.wet, for: baby)
        let med = try store.logSupplement(name: "Vitamin D", for: baby)
        let check = try store.logMeasurement(for: baby, at: clock.now, timeZoneID: "UTC",
                                             values: MeasurementValues(weightKG: 4.2))
        let model = HistoryViewModel(store: store, babyID: baby)

        #expect(try model.entryRef(for: feed.id) == .feed(feed.id))
        #expect(try model.entryRef(for: sleep.id) == .sleep(sleep.id))
        #expect(try model.entryRef(for: diaper.id) == .diaper(diaper.id))
        #expect(try model.entryRef(for: med.id) == .supplement(med.id))
        #expect(try model.entryRef(for: check.id) == .measurement(check.id))
    }

    @Test func aSourceGoneSinceAskAnsweredNamesNothing() throws {
        let (store, baby, _) = try makeStore()
        let diaper = try store.logDiaper(.wet, for: baby)
        try store.softDelete(diaper)
        let model = HistoryViewModel(store: store, babyID: baby)

        #expect(try model.entryRef(for: diaper.id) == nil)
        #expect(try model.entryRef(for: UUID()) == nil)
    }

    /// Ported from the old editor's tests: an edit made by a sheet shows on
    /// the list at its next load.
    @Test func anEditShowsAfterAReload() throws {
        let (store, baby, _) = try makeStore()
        let diaper = try store.logDiaper(.wet, for: baby)
        let history = HistoryViewModel(store: store, babyID: baby)
        try history.load(dayCount: 2)
        #expect(history.days.first?.rows.first?.text == "Wet")

        diaper.contents = .dry
        try store.update(diaper)
        try history.load(dayCount: 2)

        #expect(history.days.first?.rows.first?.text == "Dry")
    }
}
