import Foundation
import Testing
import BabyCore
import BabyStore
@testable import InsightsFeature

@Suite @MainActor struct InsightsGrowthTests {
    @Test func weightCardUsesOnlyWeightAndRefreshesAfterStoreWrites() throws {
        let now = Date(timeIntervalSince1970: 1_700_000_000)
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
        let baby = try store.addBaby(name: "Mira", birthDate: now.addingTimeInterval(-864000), referenceSex: .girls, expectedDueDate: nil)
        let model = InsightsViewModel(store: store, babyID: baby.id)
        try model.load()
        #expect(model.growth.summary(for: .weight).points.isEmpty)
        #expect(model.growth.summary(for: .weight).chartPoints.isEmpty)
        for offset in [120.0, 60.0] {
            try store.logMeasurement(for: baby.id, at: now.addingTimeInterval(-offset), timeZoneID: "UTC",
                values: MeasurementValues(lengthCM: 52, headCircumferenceCM: 35, lengthPosition: .lying))
        }
        try model.load()
        #expect(model.growth.summary(for: .weight).points.isEmpty)
        try store.logMeasurement(for: baby.id, at: now.addingTimeInterval(-60), timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        try model.load()
        #expect(model.growth.summary(for: .weight).latest?.value == 4)
        #expect(model.growth.summary(for: .weight).chartPoints.isEmpty)
        let before = store.revision
        let second = try store.logMeasurement(for: baby.id, at: now, timeZoneID: "UTC", values: MeasurementValues(weightKG: 5))
        #expect(store.revision != before)
        try model.load()
        #expect(model.growth.summary(for: .weight).chartPoints.map(\.value) == [4, 5])
        #expect(model.growth.summary(for: .weight).previousChange == 1)
        #expect(model.growth.checkInCount == 4)
        #expect(model.growth.birthDate == baby.birthDate)
        try store.softDelete(second)
        try model.load()
        #expect(model.growth.summary(for: .weight).chartPoints.isEmpty)
        #expect(model.growth.summary(for: .weight).latest?.value == 4)
    }
}
