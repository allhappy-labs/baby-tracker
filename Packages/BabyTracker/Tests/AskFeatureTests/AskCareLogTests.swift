import Foundation
import Testing
import BabyCore
import BabyStore
@testable import AskFeature

private let zurich = TimeZone(identifier: "Europe/Zurich")!

private func local(_ day: Int, _ hour: Int, _ minute: Int = 0) -> Date {
    var calendar = Calendar(identifier: .gregorian)
    calendar.timeZone = zurich
    return calendar.date(from: DateComponents(year: 2026, month: 9, day: day, hour: hour, minute: minute))!
}

@MainActor
private func fixture() throws -> (BabyStore, UUID, AskCareLog) {
    let now = local(29, 3)
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
    let baby = try store.addBaby(name: "Marc", birthDate: now.addingTimeInterval(-120 * 86_400),
                                 referenceSex: .girls, expectedDueDate: nil)
    let log = AskCareLog(store: store, babyID: baby.id, now: now, timeZone: zurich, volumeUnit: .millilitres)
    return (store, baby.id, log)
}

@MainActor
@Test func careLogListsEntriesNewestFirstInLocalTime() throws {
    let (store, baby, log) = try fixture()
    let breast = try store.logBreast(side: .left, from: local(28, 20), to: local(28, 20, 12), for: baby)
    let diaper = try store.logDiaper(.wet, for: baby, at: local(28, 22))
    let bottle = try store.logBottle(volumeML: 80, contents: .formula, for: baby, at: local(29, 1))
    let output = log.log(range: "last 24 hours", date: "", kind: "all")
    #expect(output.split(separator: "\n") == [
        "Logged entries from Mon 28 Sep, 03:00 to Tue 29 Sep, 03:00:",
        "Summary: feed entries 2, newest Tue 29 Sep, 01:00; diapers 1 (wet 1), newest Mon 28 Sep, 22:00.",
        "Tue 29 Sep, 01:00 · feed · formula bottle 80 ml",
        "Mon 28 Sep, 22:00 · diaper · wet",
        "Mon 28 Sep, 20:00–20:12 · feed · left breast 12 min",
    ])
    #expect(log.record.sources == [.entry(kind: "feed", id: bottle.id), .entry(kind: "diaper", id: diaper.id),
                                   .entry(kind: "feed", id: breast.id)])
    #expect(log.record.queriedRange == nil)
}

@MainActor
@Test func careLogNeverReadsAnotherChild() throws {
    let (store, _, log) = try fixture()
    let other = try store.addBaby(name: "Other", birthDate: local(1, 0), referenceSex: .girls, expectedDueDate: nil)
    _ = try store.logBottle(volumeML: 500, contents: .formula, for: other.id, at: local(29, 1))
    let output = log.log(range: "last 24 hours", date: "", kind: "all")
    #expect(output == "No entries logged from Mon 28 Sep, 03:00 to Tue 29 Sep, 03:00.")
    #expect(log.record.queriedRange == AskWindow(start: local(28, 3), end: local(29, 3)))
}

@MainActor
@Test func careLogCapsLinesAndSaysHowManyMore() throws {
    let (store, baby, log) = try fixture()
    for minute in 0..<17 { _ = try store.logDiaper(.wet, for: baby, at: local(28, 10, minute)) }
    let lines = log.log(range: "yesterday", date: "", kind: "diaper").split(separator: "\n")
    #expect(lines.count == 18)
    #expect(lines[1] == "Summary: diapers 17 (wet 17), newest Mon 28 Sep, 10:16.")
    #expect(lines[2] == "Mon 28 Sep, 10:16 · diaper · wet")
    #expect(lines.last == "2 more entries not shown.")
    #expect(log.record.sources.isEmpty)
    #expect(log.record.queriedRange != nil)
}

@MainActor
@Test func badArgumentsGetACorrectionInsteadOfAnError() throws {
    let (_, _, log) = try fixture()
    #expect(log.log(range: "fortnight", date: "", kind: "all")
        == "Use range today, yesterday, last 24 hours, last 3 days, last 7 days, previous 7 days, last 30 days or date.")
    #expect(log.log(range: "date", date: "28 Sep", kind: "all") == "Use YYYY-MM-DD for date.")
    #expect(log.log(range: "date", date: "2026-09-30", kind: "all") == "That date is in the future.")
    #expect(log.log(range: "yesterday", date: "", kind: "bath") == "Use kind feed, sleep, diaper, med or all.")
    #expect(log.totals(range: "date", date: "2025-01-01", byDay: false)
        == "The log tool reads at most one year back.")
    #expect(log.record.windows.isEmpty)
}

@MainActor
@Test func careTotalsCountJoinedFeedsOnceAndSplitBottleMilk() throws {
    let (store, baby, log) = try fixture()
    _ = try store.logBottle(volumeML: 70, contents: .formula, for: baby, at: local(28, 2))
    _ = try store.logBottle(volumeML: 90, contents: .breastMilk, for: baby, at: local(28, 12))
    // 20 minutes later: joins the 12:00 feed (break limit 30 min), so one feed.
    _ = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: local(28, 12, 20))
    _ = try store.logBottle(volumeML: 80, contents: .formula, for: baby, at: local(29, 1))
    _ = try store.logDiaper(.wet, for: baby, at: local(28, 8))
    _ = try store.logDiaper(.dirty, for: baby, at: local(28, 9))
    _ = try store.logSleep(from: local(28, 13), to: local(28, 14, 30), for: baby)
    let output = log.totals(range: "last 3 days", date: "", byDay: true)
    #expect(output.split(separator: "\n") == [
        "Sun 27 Sep: 0 feeds, no bottles, 0 sleeps, 0 diapers, 0 meds",
        "Mon 28 Sep: 2 feeds, bottles 220 ml (breast milk 90 ml, formula 130 ml), 1 sleep 1 h 30 min, 2 diapers (wet 1, dirty 1), 0 meds",
        "Tue 29 Sep (partial): 1 feed, bottles 80 ml (formula 80 ml), 0 sleeps, 0 diapers, 0 meds",
        "Total: 3 feeds, bottles 300 ml (breast milk 90 ml, formula 210 ml), 1 sleep 1 h 30 min, 2 diapers (wet 1, dirty 1), 0 meds",
    ])
    let steps = try store.measuredBottleSteps(for: baby, from: local(28, 0), to: local(29, 3))
    #expect(steps.reduce(0) { $0 + $1.volumeML } == 300)
    #expect(log.record.usedTotals)
    #expect(log.record.sources.isEmpty)
}

@MainActor
@Test func careTotalsMarkPartialDaysAndCapDayLines() throws {
    let (_, _, log) = try fixture()
    let lines = log.totals(range: "last 3 days", date: "", byDay: true).split(separator: "\n")
    #expect(lines.map { String($0.split(separator: ":")[0]) }
        == ["Sun 27 Sep", "Mon 28 Sep", "Tue 29 Sep (partial)", "Total"])
    // A month read by day still gets data: one total, with a note.
    #expect(log.totals(range: "last 30 days", date: "", byDay: true)
        == "Mon 31 Aug, 00:00 to Tue 29 Sep, 03:00: 0 feeds, no bottles, 0 sleeps, 0 diapers, 0 meds\nDaily lines omitted: 30 days is more than 14.")
}

@MainActor
@Test func lastEntriesGiveTheNewestOfEachKindWithAge() throws {
    let now = Date(timeIntervalSince1970: 1_800_000_000)
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
    let baby = try store.addBaby(name: "Marc", birthDate: now.addingTimeInterval(-200 * 86_400),
                                 referenceSex: .girls, expectedDueDate: nil)
    let newest = try store.logDiaper(.wet, for: baby.id, at: now.addingTimeInterval(-31 * 86_400))
    _ = try store.logDiaper(.dirty, for: baby.id, at: now.addingTimeInterval(-45 * 86_400))
    let bottle = try store.logBottle(volumeML: 80, contents: .formula, for: baby.id,
                                     at: now.addingTimeInterval(-10 * 86_400))
    let log = AskCareLog(store: store, babyID: baby.id, now: now, timeZone: .gmt, volumeUnit: .millilitres)
    let lines = log.lastEntries().split(separator: "\n").map(String.init)
    #expect(lines.count == 4)
    #expect(lines[0].hasPrefix("feed, 10 days ago: ") && lines[0].hasSuffix("· feed · formula bottle 80 ml"))
    #expect(lines[1] == "sleep: none logged in the past year")
    #expect(lines[2].hasPrefix("diaper, 31 days ago: ") && lines[2].hasSuffix("· diaper · wet"))
    #expect(lines[3] == "med: none logged in the past year")
    #expect(log.record.sources == [.entry(kind: "feed", id: bottle.id), .entry(kind: "diaper", id: newest.id)])
}

@MainActor
@Test func toolCallsStopAfterSixPerAnswer() throws {
    let (_, _, log) = try fixture()
    for _ in 0..<6 { #expect(log.lastEntries() != AskCareLog.limitReached) }
    #expect(log.lastEntries() == AskCareLog.limitReached)
    #expect(log.record.toolNames.count == 6)
}

@MainActor
@Test func namedRangesResolveToLocalWindows() throws {
    let (_, _, log) = try fixture()
    func window(_ range: String, _ date: String = "") -> AskWindow? { log.window(range: range, date: date) }
    #expect(window("today") == AskWindow(start: local(29, 0), end: local(29, 3)))
    #expect(window("yesterday") == AskWindow(start: local(28, 0), end: local(29, 0)))
    #expect(window("last 24 hours") == AskWindow(start: local(28, 3), end: local(29, 3)))
    #expect(window("last 3 days") == AskWindow(start: local(27, 0), end: local(29, 3)))
    #expect(window("last 7 days") == AskWindow(start: local(23, 0), end: local(29, 3)))
    #expect(window("previous 7 days") == AskWindow(start: local(16, 0), end: local(23, 0)))
    #expect(window("last 30 days")?.end == local(29, 3))
    #expect(window("date", "2026-09-27") == AskWindow(start: local(27, 0), end: local(28, 0)))
    #expect(window("date", "2026-09-29") == AskWindow(start: local(29, 0), end: local(29, 3)))
    #expect(window("date", "2026-09-30") == nil)
}

@MainActor
@Test func plannedReadsRunTheChosenToolUnderALabel() throws {
    let (store, baby, log) = try fixture()
    _ = try store.logDiaper(.wet, for: baby, at: local(28, 22))
    #expect(log.run(AskRead(tool: "totals", range: "yesterday", kind: "all"))
        == "[totals, yesterday]\nMon 28 Sep, 00:00 to Tue 29 Sep, 00:00: 0 feeds, no bottles, 0 sleeps, 1 diaper (wet 1), 0 meds")
    #expect(log.run(AskRead(tool: "entries", range: "date", date: "2026-09-28", kind: "diaper"))
        == "[entries, 2026-09-28, diaper]\nLogged entries from Mon 28 Sep, 00:00 to Tue 29 Sep, 00:00:\nSummary: diapers 1 (wet 1), newest Mon 28 Sep, 22:00.\nMon 28 Sep, 22:00 · diaper · wet")
    #expect(log.run(AskRead(tool: "latest")).hasPrefix("[latest entries]\nfeed: none logged"))
    #expect(log.record.toolNames == ["careTotals", "careLog", "lastEntries"])
}

@MainActor
@Test func entrySummaryNamesTheLongestSleep() throws {
    let (store, baby, log) = try fixture()
    _ = try store.logSleep(from: local(28, 1), to: local(28, 2), for: baby)
    _ = try store.logSleep(from: local(28, 13), to: local(28, 16), for: baby)
    _ = try store.logSleep(from: local(28, 20), to: local(28, 20, 40), for: baby)
    let lines = log.log(range: "yesterday", date: "", kind: "sleep").split(separator: "\n")
    #expect(lines[1] == "Summary: sleeps 3, newest Mon 28 Sep, 20:00–20:40, longest 3 h (Mon 28 Sep, 13:00–16:00).")
}

@Test func plannedReadsDropDuplicates() {
    let plan = AskPlan(reads: [
        AskRead(tool: "entries", range: "last 24 hours", date: "2026-10-04", kind: "diaper"),
        AskRead(tool: "entries", range: "last 24 hours", date: "2026-10-03", kind: "diaper"),
        AskRead(tool: "totals", range: "last 7 days", date: "2026-10-02", kind: "feed", byDay: true),
        AskRead(tool: "totals", range: "last 7 days", date: "2026-09-25", kind: "sleep", byDay: true),
        AskRead(tool: "totals", range: "date", date: "2026-10-03"),
        AskRead(tool: "totals", range: "date", date: "2026-10-02"),
    ])
    #expect(plan.uniqueReads == [
        AskRead(tool: "entries", range: "last 24 hours", date: "2026-10-04", kind: "diaper"),
        AskRead(tool: "totals", range: "last 7 days", date: "2026-10-02", kind: "feed", byDay: true),
        AskRead(tool: "totals", range: "date", date: "2026-10-03"),
        AskRead(tool: "totals", range: "date", date: "2026-10-02"),
    ])
}

@MainActor
@Test func aSleepJoinedAcrossMidnightCountsOnTheDayItBegan() throws {
    let (store, baby, log) = try fixture()
    _ = try store.logSleep(from: local(27, 23), to: local(27, 23, 50), for: baby)
    // Ten minutes later: joins the 23:00 sleep (break limit 15 min).
    _ = try store.logSleep(from: local(28, 0), to: local(28, 2), for: baby)
    let lines = log.totals(range: "last 3 days", date: "", byDay: true).split(separator: "\n")
    #expect(lines[0] == "Sun 27 Sep: 0 feeds, no bottles, 1 sleep 2 h 50 min, 0 diapers, 0 meds")
    #expect(lines[1] == "Mon 28 Sep: 0 feeds, no bottles, 0 sleeps, 0 diapers, 0 meds")
}
