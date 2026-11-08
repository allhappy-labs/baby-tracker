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
    #expect(log.log(range: "yesterday", date: "", kind: "bath") == "Use kind feed, sleep, diaper, med, vaccine, pump or all.")
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
    #expect(lines.count == 6)
    #expect(lines[4] == "pump: none logged in the past year")
    #expect(lines[5] == "vaccine: none logged")
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

// MARK: - Vaccinations

/// Now is Tue 29 Sep 2026, 03:00 in Zurich; the baby is 300 days old.
@MainActor
private func vaccineFixture() throws -> (BabyStore, UUID, AskCareLog, Date) {
    let now = local(29, 3)
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
    let baby = try store.addBaby(name: "Marc", birthDate: now.addingTimeInterval(-300 * 86_400),
                                 referenceSex: .girls, expectedDueDate: nil)
    let log = AskCareLog(store: store, babyID: baby.id, now: now, timeZone: zurich, volumeUnit: .millilitres)
    return (store, baby.id, log, now)
}

@MainActor
@Test func aVaccineReadListsEveryShotWhateverTheRange() throws {
    let (store, baby, log, now) = try vaccineFixture()
    let first = try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby, at: local(29, 3).addingTimeInterval(-200 * 86_400))
    let second = try store.logImmunization(vaccine: "Prevenar 13", dose: 2, for: baby, at: local(29, 3).addingTimeInterval(-140 * 86_400),
                                           note: "fever that evening")
    let mmr = try store.logImmunization(vaccine: "MMR", dose: nil, for: baby, at: local(26, 10))
    _ = try store.logBottle(volumeML: 80, contents: .formula, for: baby, at: local(29, 1))

    let lines = log.log(range: "today", date: "", kind: "vaccine").split(separator: "\n").map(String.init)

    #expect(lines.count == 5)
    // The booklet spans months: its header says which year each end is in.
    #expect(lines[0].hasPrefix("Logged entries from ") && lines[0].contains(" 2026, ")
            && lines[0].hasSuffix(" to Tue 29 Sep 2026, 03:00:"))
    #expect(lines[1] == "Summary: vaccines 3, newest Sat 26 Sep, 10:00.")
    #expect(lines[2] == "Sat 26 Sep 2026, 10:00 · vaccine · MMR")
    #expect(lines[3].hasSuffix(" · vaccine · Prevenar 13 · dose 2 · note: \"fever that evening\""))
    #expect(lines[4].hasSuffix(" · vaccine · Prevenar 13 · dose 1"))
    // Each line carries its year: shots are months apart.
    #expect(lines[3].contains(" 2026, ") && lines[4].contains(" 2026, "))
    #expect(!lines.contains { $0.contains("· feed ·") })
    #expect(log.record.sources == [.entry(kind: "immunization", id: mmr.id), .entry(kind: "immunization", id: second.id),
                                   .entry(kind: "immunization", id: first.id)])
    #expect(log.record.windows.first?.end == now)
}

/// A whole booklet fits: a vaccine read is not cut at the 15 lines other reads show.
@MainActor
@Test func aVaccineReadShowsAWholeBooklet() throws {
    let (store, baby, log, _) = try vaccineFixture()
    for n in 0..<20 {
        _ = try store.logImmunization(vaccine: "Vaccine \(n)", dose: 1, for: baby, at: local(29, 3).addingTimeInterval(Double(-10 * (n + 1)) * 86_400))
    }
    let lines = log.log(range: "last 7 days", date: "", kind: "vaccine").split(separator: "\n")
    #expect(lines.count == 22)
    #expect(!lines.contains { $0.hasSuffix("more entries not shown.") })
}

@MainActor
@Test func aVaccineReadWithNoShotsSaysSo() throws {
    let (_, _, log, _) = try vaccineFixture()
    #expect(log.log(range: "today", date: "", kind: "vaccine").hasPrefix("No vaccine entries logged from "))
}

@MainActor
@Test func theKindListNamesVaccine() throws {
    let (_, _, log, _) = try vaccineFixture()
    #expect(log.log(range: "today", date: "", kind: "bath") == "Use kind feed, sleep, diaper, med, vaccine, pump or all.")
}

/// The last vaccination is found however old it is: shots are often more than a year apart.
@MainActor
@Test func latestIncludesTheLastVaccine() throws {
    let (store, baby, log, _) = try vaccineFixture()
    #expect(log.lastEntries().split(separator: "\n").last == "vaccine: none logged")
    let old = try store.logImmunization(vaccine: "Hep B", dose: 1, for: baby, at: local(29, 3).addingTimeInterval(-290 * 86_400))
    let lines = log.lastEntries().split(separator: "\n").map(String.init)
    #expect(lines.count == 6)
    #expect(lines[5].hasPrefix("vaccine, 290 days ago: ") && lines[5].hasSuffix(" · vaccine · Hep B · dose 1"))
    #expect(log.record.sources.contains(.entry(kind: "immunization", id: old.id)))
}

@MainActor
@Test func totalsCountVaccinesOnlyWhenThereAreSome() throws {
    let (store, baby, log, _) = try vaccineFixture()
    _ = try store.logBottle(volumeML: 80, contents: .formula, for: baby, at: local(29, 1))
    let without = log.totals(range: "today", date: "", byDay: false)
    #expect(without.hasSuffix(", 0 meds"))
    _ = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby, at: local(29, 2))
    _ = try store.logImmunization(vaccine: "Prevenar 13", dose: 3, for: baby, at: local(29, 2, 5))
    #expect(log.totals(range: "today", date: "", byDay: false) == without + ", 2 vaccines (Prevenar 13, MMR)")
}

@MainActor
@Test func anAllReadIncludesAShotInItsWindow() throws {
    let (store, baby, log, _) = try vaccineFixture()
    _ = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby, at: local(29, 2))
    _ = try store.logImmunization(vaccine: "Hep B", dose: 1, for: baby, at: local(1, 2))
    let lines = log.log(range: "today", date: "", kind: "all").split(separator: "\n").map(String.init)
    #expect(lines.count == 3)
    #expect(lines[1] == "Summary: vaccines 1, newest Tue 29 Sep, 02:00.")
    #expect(lines[2] == "Tue 29 Sep 2026, 02:00 · vaccine · MMR · dose 1")
}

/// A birth dose given earlier on the birth day than the stored birth instant,
/// and a shot left before the birth date by a later birth-date edit, are both
/// on the booklet, so both are in the read; and the range the model names does
/// not matter, even one that would be refused for another kind.
@MainActor
@Test func aVaccineReadIncludesTheBirthDayAndIgnoresABadRange() throws {
    let (store, babyID, log, now) = try vaccineFixture()
    let baby = try #require(try store.baby(babyID))
    let birthDose = try store.logImmunization(vaccine: "Hep B", dose: 1, for: babyID, at: Calendar.current.startOfDay(for: baby.birthDate))
    let later = try store.logImmunization(vaccine: "Hep B", dose: 2, for: babyID, at: now.addingTimeInterval(-100 * 86_400))
    // The birth date is corrected to a week later: the first dose is now before it.
    try store.updateBabyAndGrowthProfile(baby, name: baby.name, birthDate: baby.birthDate.addingTimeInterval(7 * 86_400),
                                         referenceSex: .girls, expectedDueDate: nil)

    let lines = log.log(range: "date", date: "not a date", kind: "vaccine").split(separator: "\n").map(String.init)

    #expect(lines.count == 4)
    #expect(lines[1].hasPrefix("Summary: vaccines 2, "))
    #expect(lines[2].hasSuffix(" · vaccine · Hep B · dose 2"))
    #expect(lines[3].hasSuffix(" · vaccine · Hep B · dose 1"))
    #expect(log.record.sources == [.entry(kind: "immunization", id: later.id), .entry(kind: "immunization", id: birthDose.id)])
}
