import Foundation
import Testing
import BabyCore
import BabyStore
@testable import GrowthFeature

@Suite @MainActor struct GrowthViewModelTests {
    private func fixture() throws -> (BabyStore, Baby, Date) {
        let now = Date(timeIntervalSince1970: 1_700_000_000)
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
        let baby = try store.addBaby(name: "Mira", birthDate: now.addingTimeInterval(-864000),
                                    referenceSex: .girls, expectedDueDate: nil)
        return (store, baby, now)
    }

    @Test(arguments: GrowthMetric.allCases)
    func chartsRequireTwoReadingsOfTheirOwnMetric(_ metric: GrowthMetric) throws {
        let (store, baby, now) = try fixture()
        let model = GrowthViewModel(store: store, babyID: baby.id)
        try model.load()
        #expect(model.summary(for: metric).points.isEmpty)
        #expect(model.summary(for: metric).latest == nil)
        #expect(model.summary(for: metric).previousChange == nil)
        #expect(model.summary(for: metric).chartPoints.isEmpty)
        func values(_ value: Double) throws -> MeasurementValues {
            switch metric {
            case .weight: try MeasurementValues(weightKG: value)
            case .lengthHeight: try MeasurementValues(lengthCM: value, lengthPosition: .lying)
            case .headCircumference: try MeasurementValues(headCircumferenceCM: value)
            }
        }
        try store.logMeasurement(for: baby.id, at: now.addingTimeInterval(-3600), timeZoneID: "UTC", values: values(4))
        try model.load()
        #expect(model.summary(for: metric).points.count == 1)
        #expect(model.summary(for: metric).latest?.value == 4)
        #expect(model.summary(for: metric).previousChange == nil)
        #expect(model.summary(for: metric).chartPoints.isEmpty)
        try store.logMeasurement(for: baby.id, at: now, timeZoneID: "Europe/Zurich", values: values(5))
        try model.load()
        let summary = model.summary(for: metric)
        #expect(summary.latest?.value == 5)
        #expect(summary.latest?.measuredAt == now)
        #expect(summary.latest?.timeZoneID == "Europe/Zurich")
        #expect(summary.previousChange == 1)
        #expect(summary.chartPoints.map(\.value) == [4, 5])
        for other in GrowthMetric.allCases where other != metric {
            #expect(model.summary(for: other).points.isEmpty)
        }
    }

    @Test func sparseAndCombinedCheckInsRemainIndependent() throws {
        let (store, baby, now) = try fixture()
        let model = GrowthViewModel(store: store, babyID: baby.id)
        try store.logMeasurement(for: baby.id, at: now.addingTimeInterval(-7200), timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        try store.logMeasurement(for: baby.id, at: now.addingTimeInterval(-3600), timeZoneID: "UTC", values: MeasurementValues(lengthCM: 52, lengthPosition: .lying))
        try model.load()
        #expect(model.summary(for: .weight).chartPoints.isEmpty)
        #expect(model.summary(for: .lengthHeight).chartPoints.isEmpty)
        let combined = try store.logMeasurement(for: baby.id, at: now, timeZoneID: "UTC",
            values: MeasurementValues(weightKG: 3.5, lengthCM: 54, headCircumferenceCM: 36, lengthPosition: .lying))
        try model.load()
        #expect(model.summary(for: .weight).chartPoints.map(\.value) == [4, 3.5])
        #expect(model.summary(for: .weight).previousChange == -0.5)
        #expect(model.summary(for: .lengthHeight).chartPoints.map(\.value) == [52, 54])
        #expect(model.summary(for: .lengthHeight).previousChange == 2)
        #expect(model.summary(for: .headCircumference).latest?.id == combined.id)
        #expect(model.summary(for: .headCircumference).chartPoints.isEmpty)
        #expect(model.summary(for: .lengthHeight).latest?.position == .lying)
    }

    @Test func tiedTimesKeepStoreIDOrderAndAllSameDayReadings() throws {
        let (store, baby, now) = try fixture()
        let first = try store.logMeasurement(for: baby.id, at: now, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let second = try store.logMeasurement(for: baby.id, at: now, timeZoneID: "UTC", values: MeasurementValues(weightKG: 5))
        let backdated = try store.logMeasurement(for: baby.id, at: now.addingTimeInterval(-60), timeZoneID: "UTC", values: MeasurementValues(weightKG: 3))
        let model = GrowthViewModel(store: store, babyID: baby.id)
        try model.load()
        let tieIDs = [first.id, second.id].sorted { $0.uuidString < $1.uuidString }
        #expect(model.summary(for: .weight).points.map(\.id) == tieIDs + [backdated.id])
        #expect(model.summary(for: .weight).chartPoints.count == 3)
        #expect(model.summary(for: .weight).latest?.id == tieIDs.first)
        try model.load()
        #expect(model.summary(for: .weight).points.map(\.id) == tieIDs + [backdated.id])
    }

    @Test func missingProfileAndBundledSwissReferencePreserveRawReadings() throws {
        let (store, baby, now) = try fixture()
        try store.logMeasurement(for: baby.id, at: now, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let model = GrowthViewModel(store: store, babyID: baby.id)
        try model.load()
        #expect(model.profileState == .configured)
        guard case .calculated(.available) = model.summary(for: .weight).latest?.reference else {
            Issue.record("Bundled Swiss reference must compare eligible weight"); return
        }
        let profile = try #require(try store.growthProfile(for: baby.id))
        store.context.delete(profile)
        try store.context.save()
        try model.load()
        #expect(model.profileState == .missing)
        #expect(model.summary(for: .weight).latest?.reference == .missingProfile)
        #expect(model.summary(for: .weight).latest?.value == 4)
    }

    @Test func headOnlyLegacyProfileKeepsRawHistoryWithoutRequestingAReference() throws {
        let (store, baby, now) = try fixture()
        let profile = try #require(try store.growthProfile(for: baby.id))
        store.context.delete(profile)
        try store.context.save()
        for (offset, value) in [(-3600.0, 35.0), (0.0, 36.0)] {
            try store.logMeasurement(for: baby.id, at: now.addingTimeInterval(offset), timeZoneID: "UTC",
                                     values: MeasurementValues(headCircumferenceCM: value))
        }
        let model = GrowthViewModel(store: store, babyID: baby.id)
        try model.load()
        let head = model.summary(for: .headCircumference)
        #expect(head.chartPoints.map(\.value) == [35, 36])
        #expect(head.points.count == 2)
        #expect(head.points.allSatisfy { $0.reference == .calculated(.unavailable(.noReferenceForMetric)) })
        #expect(GrowthMetricPresentation(summary: head).referenceBands.isEmpty)
    }

    // Synthetic results validate data plumbing only, not Swiss published accuracy.
    @Test func syntheticAvailableResultCanDescribeOnePointWithoutAChart() throws {
        let (store, baby, now) = try fixture()
        try store.logMeasurement(for: baby.id, at: now, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let model = GrowthViewModel(store: store, babyID: baby.id, reference: { input in
            input.metric == .weight && input.value == 4 && input.measuredAt == now && input.timeZoneID == "UTC"
                ? .available(percentile: 42, median: 5, p3: 2, p97: 8, correctedAge: false)
                : .unavailable(.invalidValue)
        })
        try model.load()
        #expect(model.summary(for: .weight).latest?.reference == .calculated(.available(percentile: 42, median: 5, p3: 2, p97: 8, correctedAge: false)))
        #expect(model.summary(for: .weight).chartPoints.isEmpty)
    }

    @Test func referenceCurveSpansTheChartForAChosenReference() throws {
        let (store, baby, now) = try fixture()
        for (offset, value) in [(-5 * 86_400.0, 3.6), (0.0, 4.0)] {
            try store.logMeasurement(for: baby.id, at: now.addingTimeInterval(offset), timeZoneID: "UTC",
                                     values: MeasurementValues(weightKG: value, headCircumferenceCM: 35))
        }
        let line = GrowthCentiles(p3: 1, p10: 2, p25: 3, p50: 4, p75: 5, p90: 6, p97: 7)
        var asked: [GrowthCentilesInput] = []
        let model = GrowthViewModel(store: store, babyID: baby.id, reference: { _ in .unavailable(.invalidValue) },
                                    centiles: { input in asked.append(input); return line })
        try model.load()
        let curve = model.summary(for: .weight).referenceCurve
        #expect(curve.count == 6)
        #expect(curve.first?.date == now.addingTimeInterval(-5 * 86_400))
        #expect(curve.last?.date == now)
        #expect(curve.allSatisfy { $0.centiles == line })
        #expect(zip(curve, curve.dropFirst()).allSatisfy { $0.date < $1.date })
        #expect(model.summary(for: .headCircumference).referenceCurve.isEmpty)
        #expect(asked.allSatisfy { $0.metric == .weight && $0.referenceSex == .girls && $0.birthDate == baby.localBirthDate })

        // No chosen reference, no curve.
        let profile = try #require(try store.growthProfile(for: baby.id))
        store.context.delete(profile)
        try store.context.save()
        try model.load()
        #expect(model.summary(for: .weight).referenceCurve.isEmpty)
    }

    @Test func referenceSamplesStepInWholeDaysAndEndOnTheLastReading() {
        let first = Date(timeIntervalSince1970: 1_700_000_000)
        let short = GrowthViewModel.referenceSampleDates(from: first, to: first.addingTimeInterval(2.5 * 86_400))
        #expect(short.map { $0.timeIntervalSince(first) / 86_400 } == [0, 1, 2, 2.5])
        let year = GrowthViewModel.referenceSampleDates(from: first, to: first.addingTimeInterval(365 * 86_400))
        #expect(year.count <= GrowthViewModel.referenceSampleCount + 1)
        #expect(zip(year, year.dropFirst()).dropLast().allSatisfy { $1.timeIntervalSince($0) == 8 * 86_400 })
        #expect(year.last == first.addingTimeInterval(365 * 86_400))
        #expect(GrowthViewModel.referenceSampleDates(from: first, to: first).isEmpty)
    }

    @Test func revisionsReloadBirthDueReferenceAndMeasurementEdits() throws {
        let (store, baby, now) = try fixture()
        let entry = try store.logMeasurement(for: baby.id, at: now, timeZoneID: "UTC", values: MeasurementValues(lengthCM: 52, lengthPosition: .standing))
        let birth = try #require(baby.localBirthDate)
        let due = try LocalDate("2023-11-10")
        let changedBirthDate = now.addingTimeInterval(-950400)
        let changedBirth = try LocalDate(displaying: changedBirthDate, in: .current)
        // Every branch emits a distinct synthetic percentile so stale inputs change the visible result.
        let model = GrowthViewModel(store: store, babyID: baby.id, reference: { input in
            let percentile: Double = input.birthDate == changedBirth ? 40
                : input.expectedDueDate == due ? 30 : input.referenceSex == .boys ? 20 : 10
            guard input.metric == .lengthHeight, input.position == .standing else { return .unavailable(.invalidValue) }
            return .available(percentile: percentile, median: 50, p3: 40, p97: 60, correctedAge: input.expectedDueDate != nil)
        })
        func expectPercentile(_ value: Double, corrected: Bool = false) {
            #expect(model.summary(for: .lengthHeight).latest?.reference == .calculated(.available(percentile: value, median: 50, p3: 40, p97: 60, correctedAge: corrected)))
        }
        try model.load()
        expectPercentile(10)
        #expect(baby.localBirthDate == birth)
        let revision = store.revision
        try store.setGrowthProfile(for: baby.id, referenceSex: .boys, expectedDueDate: nil)
        #expect(store.revision != revision)
        try model.load()
        expectPercentile(20)
        try store.setGrowthProfile(for: baby.id, referenceSex: .boys, expectedDueDate: due)
        try model.load()
        expectPercentile(30, corrected: true)
        try store.updateBabyAndGrowthProfile(baby, name: baby.name, birthDate: changedBirthDate, referenceSex: .boys, expectedDueDate: due)
        try model.load()
        expectPercentile(40, corrected: true)
        try store.editMeasurement(entry, at: now, timeZoneID: "UTC", values: MeasurementValues(lengthCM: 53, lengthPosition: .standing))
        try model.load()
        #expect(model.summary(for: .lengthHeight).latest?.value == 53)
        try store.softDelete(entry)
        try model.load()
        #expect(model.summary(for: .lengthHeight).points.isEmpty)
    }

    @Test func realmChangeCannotLeavePreviousBabyDataVisible() throws {
        let (store, baby, now) = try fixture()
        try store.logMeasurement(for: baby.id, at: now, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let model = GrowthViewModel(store: store, babyID: baby.id)
        try model.load()
        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        #expect(throws: GrowthStoreError.babyUnavailable) { try model.load() }
        #expect(model.summary(for: .weight).points.isEmpty)
        #expect(model.profileState == .notLoaded)
    }
}
