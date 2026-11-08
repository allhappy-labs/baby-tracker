import Testing
import Foundation
import BabyCore
@testable import BabyStore
import BabyUI
@testable import HistoryFeature

/// A local wall-clock time on a fixed June day (noon never crosses midnight).
@MainActor
private func localTime(_ hour: Int, _ minute: Int, dayOffset: Int = 0) -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15 + dayOffset
    parts.hour = hour; parts.minute = minute
    return Calendar.current.date(from: parts)!
}

@MainActor
private func makeStore() throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(localTime(12, 0))
    let store = try BabyStore.inMemory(dateProvider: clock)
    store.breakLimits = { .standard }
    let baby = try store.addBaby(name: "Mira", birthDate: localTime(12, 0, dayOffset: -90), referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id, clock)
}

@MainActor @Suite("Immunizations in History")
struct ImmunizationHistoryTests {
    @Test func aShotIsARowWithItsDoseAndOpensItsPanel() throws {
        let (store, baby, _) = try makeStore()
        _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: localTime(8, 0))
        let plain = HistoryViewModel(store: store, babyID: baby)
        try plain.load(dayCount: 2)
        let totalsWithoutShots = try #require(plain.days.first).totals

        let dosed = try store.logImmunization(vaccine: "Prevenar 13", dose: 2, for: baby, at: localTime(10, 15))
        let undosed = try store.logImmunization(vaccine: "MMR", dose: nil, for: baby, at: localTime(11, 15))
        let history = HistoryViewModel(store: store, babyID: baby)
        try history.load(dayCount: 2)
        let day = try #require(history.days.first)
        let shots = day.rows.filter { $0.tracker == .vaccine }
        #expect(shots.map(\.text) == ["MMR", "Prevenar 13 · dose 2"])
        #expect(shots.map(\.target) == [.entry(.immunization(undosed.id)), .entry(.immunization(dosed.id))])
        #expect(shots.map(\.start) == [HistoryViewModel.clockTime(localTime(11, 15), zone: TimeZone.current.identifier),
                                      HistoryViewModel.clockTime(localTime(10, 15), zone: TimeZone.current.identifier)])
        #expect(shots.allSatisfy { $0.chips.isEmpty && $0.end == nil && !$0.deleteConfirmation && !$0.isRunning })
        // A shot is not a feed, a sleep or a diaper: the day's totals do not move.
        #expect(day.totals == totalsWithoutShots)
    }

    @Test func theVaccineFilterShowsOnlyShots() throws {
        let (store, baby, _) = try makeStore()
        _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: localTime(8, 0))
        _ = try store.logDiaper(.wet, for: baby, at: localTime(9, 0))
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby, at: localTime(10, 0))
        #expect(HistoryViewModel.trackers.contains(.vaccine))
        let model = HistoryViewModel(store: store, babyID: baby)
        model.filter = [.vaccine]
        try model.load(dayCount: 2)
        #expect(model.days.flatMap(\.rows).map(\.id) == [shot.id])
        model.filter = Set(HistoryViewModel.trackers).subtracting([.vaccine])
        try model.load(dayCount: 2)
        #expect(!model.days.flatMap(\.rows).contains { $0.tracker == .vaccine })
        #expect(model.days.flatMap(\.rows).count == 2)
    }

    @Test func aSwipeDeleteTombstonesTheShot() throws {
        let (store, baby, _) = try makeStore()
        _ = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby, at: localTime(10, 0))
        let model = HistoryViewModel(store: store, babyID: baby)
        try model.load(dayCount: 2)
        let row = try #require(model.days.first?.rows.first)
        try model.delete(row)
        #expect(try store.immunizations(for: baby).isEmpty)
        #expect(model.days.isEmpty)
        #expect(model.emptyState == .noData)
    }

    @Test func aBabyWithOnlyAnOldShotHasData() throws {
        let (store, baby, _) = try makeStore()
        _ = try store.logImmunization(vaccine: "Hep B", dose: 1, for: baby, at: localTime(12, 0, dayOffset: -80))
        let model = HistoryViewModel(store: store, babyID: baby)
        try model.load(dayCount: 2)
        #expect(model.days.isEmpty)
        #expect(model.emptyState == .nothingRecent(dayCount: 2))
    }

    @Test func anAskSourceNamesTheShot() throws {
        let (store, baby, _) = try makeStore()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby, at: localTime(10, 0))
        let model = HistoryViewModel(store: store, babyID: baby)
        #expect(try model.entryRef(for: shot.id) == .immunization(shot.id))
    }
}
