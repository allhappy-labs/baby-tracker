import Testing
import Foundation
import BabyCore
@testable import BabyStore
import BabyUI
@testable import HistoryFeature
import CoreGraphics
#if canImport(UIKit)
import UIKit
import PDFKit
#endif

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
    let baby = try store.addBaby(
        name: "Mira", birthDate: localTime(12, 0, dayOffset: -90), referenceSex: .girls, expectedDueDate: nil
    )
    return (store, baby.id, clock)
}

@MainActor
private func key(_ dayOffset: Int) -> DayKey {
    DayBucket.key(for: localTime(12, 0, dayOffset: dayOffset), timeZoneID: TimeZone.current.identifier)
}

@MainActor
private func report(_ store: BabyStore, _ baby: UUID, from: Int, through: Int,
                    unit: VolumeUnit = .millilitres) throws -> HistoryReport? {
    try HistoryReportBuilder.make(store: store, babyID: baby, babyName: "Mira",
                                  from: key(from), through: key(through), volumeUnit: unit)
}

// MARK: - Report

@MainActor
@Test func aReportRunsOldestFirstAndSkipsEmptyDays() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(8, 0, dayOffset: -3))
    _ = try store.logDiaper(.dirty, for: baby, at: localTime(9, 0))
    _ = try store.logDiaper(.wet, for: baby, at: localTime(7, 0))

    let report = try #require(try report(store, baby, from: -6, through: 0))

    #expect(report.days.map(\.id) == [key(-3), key(0)])
    #expect(report.days[1].entries.map(\.details) == ["Wet", "Dirty"])
    #expect(report.days[1].totalsLine == "2 diapers")
    #expect(!report.days[1].title.contains("Today"))
}

@MainActor
@Test func aReportCoversEveryTrackerWithItsDetailsAndNotes() throws {
    let (store, baby, _) = try makeStore()
    let left = try store.logBreast(side: .left, from: localTime(6, 0), to: localTime(6, 12), for: baby)
    _ = try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: localTime(6, 20)), toSession: left.id, for: baby)
    _ = try store.logSleep(from: localTime(8, 0), to: localTime(9, 30), for: baby)
    _ = try store.logDiaper(.wet, for: baby, at: localTime(10, 0), note: "Rash on the left")
    _ = try store.logSupplement(name: "Vitamin D", for: baby, at: localTime(11, 0), note: "  ")

    let day = try #require(try report(store, baby, from: 0, through: 0)?.days.first)

    #expect(day.entries.map(\.type) == [Tracker.feed.title, Tracker.sleep.title, Tracker.diaper.title, Tracker.supplement.title])
    #expect(day.entries[0].details == "L 12m · 60 ml")
    #expect(day.entries[0].time == HistoryViewModel.clockTime(localTime(6, 0), zone: TimeZone.current.identifier)
            + "–" + HistoryViewModel.clockTime(localTime(6, 20), zone: TimeZone.current.identifier))
    #expect(day.entries[1].details == "1h 30m")
    #expect(day.entries[2].note == "Rash on the left")
    #expect(day.entries[3].details == "Vitamin D")
    #expect(day.entries[3].note == nil)
    #expect(day.totals == HistoryDayTotals(feeds: 1, sleep: "1h 30m", bottleML: 60, diapers: 1))
    #expect(day.totalsLine == "1 feed · 1h 30m sleep · 60 ml bottle · 1 diaper")
}

/// A vaccination prints as a line of its day, with its dose and its note.
@MainActor
@Test func aReportListsAVaccinationWithItsDoseAndNote() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(9, 0))
    _ = try store.logImmunization(vaccine: "Prevenar 13", dose: 2, for: baby, at: localTime(10, 15), note: "left thigh")

    let day = try #require(try report(store, baby, from: 0, through: 0)?.days.first)

    #expect(day.entries.map(\.type) == [Tracker.diaper.title, Tracker.vaccine.title])
    #expect(day.entries[1].details == "Prevenar 13 · dose 2")
    #expect(day.entries[1].note == "left thigh")
    #expect(day.totalsLine == "1 diaper")
}

/// The export ignores History's filter: it is built with every tracker.
@MainActor
@Test func aFilteredHistoryStillExportsEveryTracker() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: localTime(9, 0))
    _ = try store.logDiaper(.wet, for: baby, at: localTime(10, 0))
    let model = HistoryViewModel(store: store, babyID: baby)
    model.filter = [.diaper]
    try model.load(dayCount: 1)

    let day = try #require(try report(store, baby, from: 0, through: 0)?.days.first)
    #expect(day.entries.count == 2)
    #expect(day.totals.bottleML == 90)
}

/// The first and last day are whole local days: 00:05 on the first and
/// 23:55 on the last are in; the minutes either side are out.
@MainActor
@Test func aReportIncludesItsFirstAndLastWholeDays() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(23, 55, dayOffset: -3))
    _ = try store.logDiaper(.wet, for: baby, at: localTime(0, 5, dayOffset: -2))
    _ = try store.logDiaper(.dirty, for: baby, at: localTime(23, 55, dayOffset: -1))
    _ = try store.logDiaper(.dirty, for: baby, at: localTime(0, 5))

    let report = try #require(try report(store, baby, from: -2, through: -1))
    #expect(report.days.map(\.id) == [key(-2), key(-1)])
    #expect(report.days.flatMap(\.entries).count == 2)
}

@MainActor
@Test func aRangeWithNothingLoggedMakesNoReport() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(9, 0))
    #expect(try report(store, baby, from: -10, through: -5) == nil)
}

/// Review focus 4: the PDF speaks the user's unit.
@MainActor
@Test func aReportUsesTheVolumeUnit() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logBottle(volumeML: 236.588, contents: .formula, for: baby, at: localTime(9, 0))
    let day = try #require(try report(store, baby, from: 0, through: 0, unit: .ounces)?.days.first)
    #expect(day.entries[0].details == "8 oz")
    #expect(day.totalsLine == "1 feed · 8 oz bottle")
}

/// Review focus 5: a running timer is not a finished entry and is not printed.
@MainActor
@Test func aRunningTimerIsNotExported() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(9, 0))
    _ = try store.startSleep(for: baby)
    let day = try #require(try report(store, baby, from: 0, through: 0)?.days.first)
    #expect(day.entries.map(\.type) == [Tracker.diaper.title])
}

// MARK: - HTML

private func sampleReport(name: String = "Mira", note: String? = nil, days: Int = 1, entriesPerDay: Int = 1) -> HistoryReport {
    HistoryReport(
        babyName: name,
        rangeText: "1–7 Oct 2026",
        generatedText: "8 October 2026 at 14:05",
        days: (0..<days).map { index in
            HistoryReport.Day(
                id: DayKey(year: 2026, month: 10, day: 1 + index),
                title: "Thursday, \(1 + index) October 2026",
                totals: HistoryDayTotals(feeds: 2, sleep: "3h", bottleML: 120, diapers: 1),
                totalsLine: "2 feeds · 3h sleep · 120 ml bottle · 1 diaper",
                entries: (0..<entriesPerDay).map { _ in
                    HistoryReport.Entry(time: "10:23–11:05", type: "Feed", details: "L 12m · 60 ml", note: note)
                }
            )
        }
    )
}

@Test func escapingNeutralisesMarkup() {
    #expect(HistoryReportHTML.escape(#"<script>"a" & 'b'</script>"#)
            == "&lt;script&gt;&quot;a&quot; &amp; &#39;b&#39;&lt;/script&gt;")
}

/// Review focus 2: a name or note is text, never markup.
@Test func userTextIsEscapedInTheDocument() {
    let html = HistoryReportHTML.render(sampleReport(name: "Mia & <b>Lev</b>", note: "Spit up <a lot>\nthen slept"))
    #expect(html.contains("Mia &amp; &lt;b&gt;Lev&lt;/b&gt;"))
    #expect(html.contains("Spit up &lt;a lot&gt;<br>then slept"))
    #expect(!html.contains("<b>Lev"))
}

@Test func everyDayPrintsItsTitleTotalsAndRows() {
    let html = HistoryReportHTML.render(sampleReport(days: 2, entriesPerDay: 3))
    #expect(html.components(separatedBy: "<section").count - 1 == 2)
    #expect(html.contains("2 feeds · 3h sleep · 120 ml bottle · 1 diaper"))
    #expect(html.components(separatedBy: "<tr class=\"entry\">").count - 1 == 6)
    #expect(html.contains("display: table-header-group"))
    #expect(html.contains("1–7 Oct 2026"))
}

/// A report with no notes prints no Note column; one note anywhere brings
/// it back for every day.
@Test func theNoteColumnPrintsOnlyWhenThereIsANote() {
    let without = HistoryReportHTML.render(sampleReport(days: 2, entriesPerDay: 2))
    #expect(!without.contains("<th>Note</th>"))
    #expect(!without.contains("class=\"note\""))
    #expect(without.components(separatedBy: "<col ").count - 1 == 2 * 3)

    let with = HistoryReportHTML.render(sampleReport(note: "Fussy", days: 2, entriesPerDay: 2))
    #expect(with.contains("<th>Note</th>"))
    #expect(with.components(separatedBy: "<col ").count - 1 == 2 * 4)
    #expect(with.components(separatedBy: "<td class=\"note\">Fussy</td>").count - 1 == 4)
}

// MARK: - Files and paper

@Test func paperFollowsTheMeasurementSystem() {
    #expect(PaperSize.for(Locale(identifier: "en_US")) == .letter)
    #expect(PaperSize.for(Locale(identifier: "en_GB")) == .a4)
    #expect(PaperSize.for(Locale(identifier: "uk_UA")) == .a4)
    #expect(PaperSize.letter.size == CGSize(width: 612, height: 792))
}

/// Review focus 2: a name with path characters still makes one valid file.
@Test func theFileNameIsSafe() {
    #expect(HistoryExportFiles.fileName(babyName: "Mia/Lev: \"twins\"", rangeText: "1–7 Oct 2026")
            == "Mia-Lev- -twins- – 1–7 Oct 2026.pdf")
    #expect(HistoryExportFiles.fileName(babyName: "  ", rangeText: "1–7 Oct 2026") == "History – 1–7 Oct 2026.pdf")
}

@Test func writingAnExportClearsTheLastOne() throws {
    let folder = FileManager.default.temporaryDirectory.appendingPathComponent("history-export-test-\(UUID())")
    defer { try? FileManager.default.removeItem(at: folder) }
    let files = try HistoryExportFiles(directory: folder)

    let first = try files.write(Data("one".utf8), named: "A.pdf")
    let second = try files.write(Data("two".utf8), named: "B.pdf")

    #expect(!FileManager.default.fileExists(atPath: first.path))
    #expect(try Data(contentsOf: second) == Data("two".utf8))
    #expect(try FileManager.default.contentsOfDirectory(atPath: folder.path) == ["B.pdf"])
}

#if canImport(UIKit)
/// Review focus 1: a long report runs over several pages. Not counted by
/// `make test` (macOS); run on the shared simulator.
@MainActor
@Test func aLongReportPrintsToSeveralPages() throws {
    let data = HistoryPDFPrinter.pdf(report: sampleReport(note: String(repeating: "Long note. ", count: 30), days: 10, entriesPerDay: 12),
                                     paper: .a4)
    let provider = try #require(CGDataProvider(data: data as CFData))
    let document = try #require(CGPDFDocument(provider))
    #expect(document.numberOfPages > 1)
    let box = try #require(document.page(at: 1)).getBoxRect(.mediaBox)
    #expect(abs(box.width - 595.28) < 1)
}

/// Review focus 1: a page that starts mid-table still says what its columns
/// are, and whose record it is. UIKit's print WebKit ignores `thead`
/// repetition, so the printer draws them as a running header.
@MainActor
@Test func laterPagesRepeatTheColumnHeadings() throws {
    let report = sampleReport(name: "Mira", note: "Fussy", days: 1, entriesPerDay: 80)
    let data = HistoryPDFPrinter.pdf(report: report, paper: .a4)
    let document = try #require(PDFDocument(data: data))
    #expect(document.pageCount > 1)
    let first = try #require(document.page(at: 0)?.string)
    let second = try #require(document.page(at: 1)?.string)
    #expect(!first.contains("Page 1 of"))
    #expect(second.contains("Mira · 1–7 Oct 2026"))
    #expect(second.contains("Page 2 of \(document.pageCount)"))
    for column in ["Time", "Type", "Details", "Note"] {
        #expect(second.contains(column), "page 2 has no \(column) heading")
    }

    // The running headings sit inside the margins, over their columns:
    // "Details" starts where the rows' details start.
    let page = try #require(document.page(at: 1))
    func left(_ text: String) throws -> CGFloat {
        let match = try #require(document.findString(text, withOptions: []).first { $0.pages.contains(page) })
        return match.bounds(for: page).minX
    }
    let details = try left("Details")
    let rowDetails = try left("L 12m")
    #expect(abs(details - rowDetails) < 3, "Details heading at \(details), the column at \(rowDetails)")
    #expect(try left("Time") >= HistoryPDFPrinter.margin)
}
#endif
