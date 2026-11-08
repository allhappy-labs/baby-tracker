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

@MainActor
private func pumpRows(_ history: HistoryViewModel) -> [HistoryListRow] {
    history.days.flatMap(\.rows).filter { $0.tracker == .pump }
}

@MainActor @Suite("Pumps in History")
struct PumpHistoryTests {
    @Test func aFinishedPumpReadsLengthAmountAndSide() throws {
        let (store, baby, _) = try makeStore()
        _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: localTime(8, 0))
        let plain = HistoryViewModel(store: store, babyID: baby)
        try plain.load(dayCount: 2)
        let totalsWithoutPumps = try #require(plain.days.first).totals

        let pump = try store.logPump(side: .both, from: localTime(10, 0), to: localTime(10, 20), volumeML: 90, note: nil)
        let history = HistoryViewModel(store: store, babyID: baby)
        try history.load(dayCount: 2)
        let row = try #require(pumpRows(history).first)
        #expect(row.text == "20m · 90 ml · Both")
        #expect(row.target == .entry(.pump(pump.id)))
        #expect(row.start == HistoryViewModel.clockTime(localTime(10, 0), zone: TimeZone.current.identifier))
        #expect(row.end == HistoryViewModel.clockTime(localTime(10, 20), zone: TimeZone.current.identifier))
        #expect(row.chips.isEmpty)
        #expect(!row.isRunning)
        #expect(!row.deleteConfirmation)
        // A pump is not the baby's feed, sleep or diaper: the day's totals do not move.
        #expect(try #require(history.days.first).totals == totalsWithoutPumps)
    }

    @Test func aPumpWithoutAnAmountShowsADash() throws {
        let (store, baby, _) = try makeStore()
        _ = try store.logPump(side: .left, from: localTime(10, 0), to: localTime(10, 20), volumeML: nil, note: nil)
        let history = HistoryViewModel(store: store, babyID: baby)
        try history.load(dayCount: 2)
        #expect(pumpRows(history).first?.text == "20m · — · Left")
    }

    @Test func aRunningPumpReadsPumpingAndIsNotPrinted() throws {
        let (store, baby, clock) = try makeStore()
        let pump = try store.startPump(side: .right)
        let history = HistoryViewModel(store: store, babyID: baby)
        try history.load(dayCount: 2)
        let row = try #require(pumpRows(history).first)
        #expect(pumpRows(history).count == 1)
        #expect(row.text == "Pumping…")
        #expect(row.isRunning)
        #expect(row.runningSince == pump.startedAt)
        #expect(row.target == .entry(.pump(pump.id)))
        #expect(history.emptyState == nil)
        let today = DayBucket.key(for: clock.now, timeZoneID: TimeZone.current.identifier)
        let report = try HistoryReportBuilder.make(store: store, babyID: baby, babyName: "Mira", from: today, through: today,
                                                   volumeUnit: .millilitres)
        #expect(report == nil)
    }

    @Test func thePumpFilterHidesAndShowsPumps() throws {
        let (store, baby, _) = try makeStore()
        _ = try store.logDiaper(.wet, for: baby, at: localTime(9, 0))
        let pump = try store.logPump(side: .left, from: localTime(10, 0), to: localTime(10, 20), volumeML: 30, note: nil)
        _ = try store.startPump(side: .both)
        #expect(HistoryViewModel.trackers.contains(.pump))
        let history = HistoryViewModel(store: store, babyID: baby)
        history.filter = [.pump]
        try history.load(dayCount: 2)
        #expect(history.days.flatMap(\.rows).allSatisfy { $0.tracker == .pump })
        #expect(pumpRows(history).count == 2)
        #expect(pumpRows(history).contains { $0.id == pump.id })
        history.filter = Set(HistoryViewModel.trackers).subtracting([.pump])
        try history.load(dayCount: 2)
        #expect(pumpRows(history).isEmpty)
        #expect(history.days.flatMap(\.rows).count == 1)
    }

    /// The same session in both babies' History; deleting it from one removes
    /// it from both, and an Ask source for it resolves from either.
    @Test func pumpsShowInEveryBabysHistoryAndDeleteOnce() throws {
        let (store, mira, _) = try makeStore()
        let theo = try store.addBaby(name: "Theo", birthDate: localTime(12, 0, dayOffset: -30), referenceSex: .boys, expectedDueDate: nil).id
        let pump = try store.logPump(side: .both, from: localTime(10, 0), to: localTime(10, 20), volumeML: 60, note: nil)
        let miraHistory = HistoryViewModel(store: store, babyID: mira)
        let theoHistory = HistoryViewModel(store: store, babyID: theo)
        try miraHistory.load(dayCount: 2)
        try theoHistory.load(dayCount: 2)
        #expect(pumpRows(miraHistory).map(\.id) == [pump.id])
        #expect(pumpRows(theoHistory).map(\.id) == [pump.id])
        #expect(try theoHistory.entryRef(for: pump.id) == .pump(pump.id))
        try miraHistory.delete(try #require(pumpRows(miraHistory).first))
        try theoHistory.load(dayCount: 2)
        #expect(pumpRows(miraHistory).isEmpty)
        #expect(pumpRows(theoHistory).isEmpty)
        #expect(try store.pump(id: pump.id) == nil)
    }

    @Test func thePDFListsAFinishedPumpWithItsNote() throws {
        let (store, baby, clock) = try makeStore()
        _ = try store.logPump(side: .both, from: localTime(10, 0), to: localTime(10, 20), volumeML: 90, note: "evening")
        let today = DayBucket.key(for: clock.now, timeZoneID: TimeZone.current.identifier)
        let report = try #require(try HistoryReportBuilder.make(store: store, babyID: baby, babyName: "Mira", from: today,
                                                                through: today, volumeUnit: .millilitres))
        let entry = try #require(report.days.first?.entries.first { $0.type == "Pump" })
        #expect(entry.details == "20m · 90 ml · Both")
        #expect(entry.note == "evening")
        #expect(entry.time.contains("–"))
    }

    @Test func aFamilyWithOnlyAnOldPumpHasData() throws {
        let (store, baby, _) = try makeStore()
        _ = try store.logPump(side: .left, from: localTime(10, 0, dayOffset: -40), to: localTime(10, 20, dayOffset: -40), volumeML: nil, note: nil)
        let model = HistoryViewModel(store: store, babyID: baby)
        try model.load(dayCount: 2)
        #expect(model.days.isEmpty)
        #expect(model.emptyState == .nothingRecent(dayCount: 2))
    }
}
