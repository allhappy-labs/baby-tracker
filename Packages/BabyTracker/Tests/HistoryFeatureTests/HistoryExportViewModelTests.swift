import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import HistoryFeature

@MainActor
private func day(_ offset: Int, _ hour: Int = 0) -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15 + offset; parts.hour = hour
    return Calendar.current.date(from: parts)!
}

@MainActor
private func makeModel(bornDaysAgo: Int = 90, render: ((HistoryReport) throws -> Data)? = { _ in Data("%PDF".utf8) })
    throws -> (HistoryExportViewModel, BabyStore, UUID, URL) {
    let clock = TestDateProvider(day(0, 12))
    let store = try BabyStore.inMemory(dateProvider: clock)
    store.breakLimits = { .standard }
    let baby = try store.addBaby(name: "Mira", birthDate: day(-bornDaysAgo, 9), referenceSex: .girls, expectedDueDate: nil)
    let folder = FileManager.default.temporaryDirectory.appendingPathComponent("history-export-vm-\(UUID())")
    let model = HistoryExportViewModel(store: store, babyID: baby.id, babyName: "Mira", volumeUnit: .millilitres,
                                       files: try HistoryExportFiles(directory: folder), render: render)
    return (model, store, baby.id, folder)
}

@MainActor
@Test func presetsEndTodayAndCountTodayIn() throws {
    let (model, _, _, folder) = try makeModel()
    defer { try? FileManager.default.removeItem(at: folder) }
    #expect(model.preset == .week)
    #expect(model.from == day(-6)); #expect(model.to == day(0))
    model.select(.fortnight); #expect(model.from == day(-13)); #expect(model.to == day(0))
    model.select(.month); #expect(model.from == day(-29))
    model.select(.custom); #expect(model.from == day(-29)); #expect(model.to == day(0))
}

/// A baby younger than the preset starts on its birth day.
@MainActor
@Test func aPresetNeverReachesBeforeBirth() throws {
    let (model, _, _, folder) = try makeModel(bornDaysAgo: 3)
    defer { try? FileManager.default.removeItem(at: folder) }
    #expect(model.earliest == day(-3))
    #expect(model.from == day(-3))
}

/// Review focus 3: custom dates stay between birth and today, in order.
@MainActor
@Test func customDatesClampAndStayInOrder() throws {
    let (model, _, _, folder) = try makeModel(bornDaysAgo: 20)
    defer { try? FileManager.default.removeItem(at: folder) }
    model.select(.custom)
    model.setFrom(day(-400, 15)); #expect(model.from == day(-20))
    model.setTo(day(30, 8)); #expect(model.to == day(0))
    model.setTo(day(-10)); model.setFrom(day(-5)); #expect(model.from == day(-5)); #expect(model.to == day(-5))
    model.setFrom(day(-12)); model.setTo(day(-15)); #expect(model.from == day(-15)); #expect(model.to == day(-15))
}

@MainActor
@Test func creatingWritesTheFileAndSharesIt() async throws {
    let (model, store, baby, folder) = try makeModel()
    defer { try? FileManager.default.removeItem(at: folder) }
    _ = try store.logDiaper(.wet, for: baby, at: day(-1, 9))

    await model.create()

    guard case .ready(let url) = model.state else { Issue.record("state \(model.state)"); return }
    #expect(url.lastPathComponent.hasPrefix("Mira – "))
    #expect(url.pathExtension == "pdf")
    #expect(try Data(contentsOf: url) == Data("%PDF".utf8))
    model.shareDidFinish()
    #expect(model.state == .idle)
}

@MainActor
@Test func aRangeWithNothingSaysSoAndWritesNothing() async throws {
    let (model, _, _, folder) = try makeModel()
    defer { try? FileManager.default.removeItem(at: folder) }
    await model.create()
    #expect(model.state == .nothingToExport)
    #expect(try FileManager.default.contentsOfDirectory(atPath: folder.path).isEmpty)
}

/// "Nothing logged in these dates" speaks of the dates it was asked about;
/// choosing other dates clears it until Create PDF is tapped again.
@MainActor
@Test func aNewRangeClearsTheLastResult() async throws {
    let (model, _, _, folder) = try makeModel()
    defer { try? FileManager.default.removeItem(at: folder) }
    for change in [{ model.select(.month) }, { model.setFrom(day(-3)) }, { model.setTo(day(-1)) }] {
        await model.create()
        #expect(model.state == .nothingToExport)
        change()
        #expect(model.state == .idle)
    }
}

@MainActor
@Test func aRenderFailureIsReported() async throws {
    struct Boom: Error {}
    let (model, store, baby, folder) = try makeModel(render: { _ in throw Boom() })
    defer { try? FileManager.default.removeItem(at: folder) }
    _ = try store.logDiaper(.wet, for: baby, at: day(0, 9))
    await model.create()
    #expect(model.state == .failed)
}
