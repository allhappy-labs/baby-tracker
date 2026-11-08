import Foundation
import Testing
import BabyCore
@testable import BabyStore
@testable import AskFeature

@MainActor
private func pumpLog() throws -> (BabyStore, UUID, AskCareLog, Date) {
    let now = Date(timeIntervalSince1970: 1_800_000_000)   // 08:00 UTC
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
    let baby = try store.addBaby(name: "Marc", birthDate: now.addingTimeInterval(-200 * 86_400), referenceSex: .girls, expectedDueDate: nil)
    let log = AskCareLog(store: store, babyID: baby.id, now: now, timeZone: .gmt, volumeUnit: .millilitres)
    return (store, baby.id, log, now)
}

@MainActor
@Test func pumpTotalsCountFinishedSessionsAndTheirAmount() throws {
    let (store, _, log, now) = try pumpLog()
    _ = try store.logPump(side: .left, from: now.addingTimeInterval(-7200), to: now.addingTimeInterval(-6600), volumeML: 60, note: nil)
    _ = try store.logPump(side: .right, from: now.addingTimeInterval(-3600), to: now.addingTimeInterval(-3000), volumeML: nil, note: nil)
    _ = try store.startPump(side: .both)
    let total = try #require(log.totals(range: "today", date: "", byDay: false).split(separator: "\n").last)
    #expect(total.hasSuffix(", 0 meds, 2 pumping sessions 60 ml pumped"))
}

@MainActor
@Test func aDayWithoutPumpingKeepsItsTotalsLine() throws {
    let (_, _, log, _) = try pumpLog()
    let total = try #require(log.totals(range: "today", date: "", byDay: false).split(separator: "\n").last)
    #expect(total.hasSuffix(", 0 meds"))
}

@MainActor
@Test func entriesOfKindPumpListOnlyPumpsWithASource() throws {
    let (store, baby, log, now) = try pumpLog()
    let pump = try store.logPump(side: .both, from: now.addingTimeInterval(-1800), to: now.addingTimeInterval(-600), volumeML: 90, note: "evening")
    _ = try store.logDiaper(.wet, for: baby, at: now.addingTimeInterval(-300))
    let lines = log.log(range: "today", date: "", kind: "pump").split(separator: "\n").map(String.init)
    let entries = lines.filter { $0.contains(" · pump · ") }
    #expect(entries.count == 1)
    #expect(entries.first?.contains(" · pump · both · 20 min · 90 ml") == true)
    #expect(entries.first?.contains("evening") == true)
    #expect(!lines.contains { $0.contains(" · diaper · ") })
    #expect(lines.contains { $0.hasPrefix("Summary: ") && $0.contains("pumping sessions 1") })
    #expect(log.record.sources == [.entry(kind: "pump", id: pump.id)])
}

@MainActor
@Test func aRunningPumpIsListedAsRunningWithNoAmount() throws {
    let (store, _, log, now) = try pumpLog()
    _ = try store.startPump(side: .left, at: now.addingTimeInterval(-600))
    let lines = log.log(range: "today", date: "", kind: "all").split(separator: "\n").map(String.init)
    #expect(lines.contains { $0.contains(" · pump · left · running · no amount") })
}

@MainActor
@Test func latestNamesTheLastPump() throws {
    let (store, _, log, now) = try pumpLog()
    #expect(log.lastEntries().split(separator: "\n").map(String.init)[4] == "pump: none logged in the past year")
    let pump = try store.logPump(side: .left, from: now.addingTimeInterval(-9000), to: now.addingTimeInterval(-8000), volumeML: 50, note: nil)
    let lines = log.lastEntries().split(separator: "\n").map(String.init)
    #expect(lines.count == 6)
    #expect(lines[4].hasPrefix("pump, "))
    #expect(lines[4].contains(" · pump · left"))
    #expect(lines[5] == "vaccine: none logged")
    #expect(log.record.sources.contains(.entry(kind: "pump", id: pump.id)))
}

@MainActor
@Test func theKindListNamesPump() throws {
    let (_, _, log, _) = try pumpLog()
    #expect(log.log(range: "today", date: "", kind: "bath") == "Use kind feed, sleep, diaper, med, vaccine, pump or all.")
}

@Test func theHeaderNamesARunningPump() {
    let now = Date(timeIntervalSince1970: 1_800_000_000)
    let text = AskText(timeZone: .gmt)
    let header = AskSessionBuilder.header(now: now, timers: [], text: text,
                                          pump: AskLivePump(startedAt: now.addingTimeInterval(-1200), side: .both))
    #expect(header.contains("No sleep or feed timer is running."))
    #expect(header.contains("Pumping running since"))
    #expect(header.contains("(20 min), both sides"))
    #expect(!AskSessionBuilder.header(now: now, timers: [], text: text).contains("Pumping"))
    let left = AskSessionBuilder.header(now: now, timers: [], text: text,
                                        pump: AskLivePump(startedAt: now.addingTimeInterval(-60), side: .left))
    #expect(left.contains("left side"))
}
