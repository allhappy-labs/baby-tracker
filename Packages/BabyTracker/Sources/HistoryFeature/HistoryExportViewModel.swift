import Foundation
import Observation
import BabyCore
import BabyStore

/// The export sheet's range choices. A preset is that many days ending
/// today, today included.
enum HistoryExportPreset: Int, CaseIterable, Identifiable {
    case week = 7, fortnight = 14, month = 30, custom = 0
    var id: Int { rawValue }
}

enum HistoryExportState: Equatable {
    case idle
    case working
    /// Nothing was logged in the chosen dates; no file was made.
    case nothingToExport
    case failed
    /// The PDF is written and waiting for the share sheet.
    case ready(URL)
}

/// Picks the dates, builds the report, prints it and hands the file to the
/// share sheet. Dates are whole local days, never before the baby's birth
/// day nor after today, and From never passes To.
@MainActor
@Observable
final class HistoryExportViewModel {
    private(set) var preset: HistoryExportPreset = .week
    private(set) var from: Date
    private(set) var to: Date
    private(set) var state: HistoryExportState = .idle
    let earliest: Date
    let latest: Date

    private let store: BabyStore
    private let babyID: UUID
    private let babyName: String
    private let volumeUnit: VolumeUnit
    private let zone = TimeZone.current.identifier
    private let files: HistoryExportFiles?
    private let render: (HistoryReport) throws -> Data

    init(store: BabyStore, babyID: UUID, babyName: String, volumeUnit: VolumeUnit,
         files: HistoryExportFiles? = nil, render: ((HistoryReport) throws -> Data)? = nil) {
        self.store = store
        self.babyID = babyID
        self.babyName = babyName
        self.volumeUnit = volumeUnit
        self.files = files ?? (try? HistoryExportFiles())
        self.render = render ?? Self.printPDF
        let zone = TimeZone.current.identifier
        func startOfDay(_ date: Date) -> Date {
            DayBucket.bounds(of: DayBucket.key(for: date, timeZoneID: zone), timeZoneID: zone).start
        }
        let latest = startOfDay(store.now)
        let birth = (try? store.baby(babyID))?.birthDate
        let earliest = birth.map { min(startOfDay($0), latest) } ?? .distantPast
        self.latest = latest
        self.earliest = earliest
        self.to = latest
        self.from = latest
        select(.week)
    }

    func select(_ preset: HistoryExportPreset) {
        self.preset = preset
        clearResult()
        guard preset != .custom else { return }
        to = latest
        from = clamp(calendar.date(byAdding: .day, value: -(preset.rawValue - 1), to: latest) ?? latest)
    }

    func setFrom(_ date: Date) {
        from = clamp(date)
        if from > to { to = from }
        clearResult()
    }

    func setTo(_ date: Date) {
        to = clamp(date)
        if to < from { from = to }
        clearResult()
    }

    /// "Nothing logged" and "Couldn't create" spoke of the dates they were
    /// asked about; other dates have not been tried yet.
    private func clearResult() {
        if state == .nothingToExport || state == .failed { state = .idle }
    }

    func create() async {
        state = .working
        // Lets the sheet draw its spinner before the main actor is busy
        // laying out pages.
        await Task.yield()
        do {
            guard let report = try HistoryReportBuilder.make(
                store: store, babyID: babyID, babyName: babyName,
                from: key(from), through: key(to), volumeUnit: volumeUnit, zone: zone
            ) else {
                state = .nothingToExport
                return
            }
            guard let files else { throw CocoaError(.fileWriteUnknown) }
            let data = try render(report)
            let name = HistoryExportFiles.fileName(babyName: babyName, rangeText: report.rangeText)
            state = .ready(try files.write(data, named: name))
        } catch {
            state = .failed
        }
    }

    func shareDidFinish() {
        if case .ready = state { state = .idle }
    }

    /// Day arithmetic in the same calendar and zone as `DayBucket`, so a
    /// preset across a DST change still lands on a local midnight.
    private var calendar: Calendar {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: zone) ?? .gmt
        return calendar
    }

    /// The start of the date's local day, between birth and today.
    private func clamp(_ date: Date) -> Date {
        let day = DayBucket.bounds(of: key(date), timeZoneID: zone).start
        return min(max(day, earliest), latest)
    }

    private func key(_ date: Date) -> DayKey { DayBucket.key(for: date, timeZoneID: zone) }

    private static func printPDF(_ report: HistoryReport) throws -> Data {
        #if canImport(UIKit)
        HistoryPDFPrinter.pdf(report: report, paper: .for(.current))
        #else
        throw CocoaError(.featureUnsupported)
        #endif
    }
}
