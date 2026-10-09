import Testing
@testable import BabyCore

@Suite("Measurement values")
struct MeasurementValuesTests {
    @Test func acceptsPartialAndCombinedCheckInsWithoutChangingRecordedValues() throws {
        let weight = try MeasurementValues(weightKG: 7.125)
        #expect(weight.weightKG == 7.125)
        #expect(weight.lengthCM == nil)
        #expect(weight.lengthPosition == nil)
        let length = try MeasurementValues(lengthCM: 65.5, lengthPosition: .standing)
        #expect(length.lengthCM == 65.5)
        #expect(length.lengthPosition == .standing)
        let head = try MeasurementValues(headCircumferenceCM: 41.75)
        #expect(head.headCircumferenceCM == 41.75)
        let combined = try MeasurementValues(weightKG: 7.125, lengthCM: 65.5,
            headCircumferenceCM: 41.75, lengthPosition: .lying)
        #expect(combined.weightKG == weight.weightKG)
        #expect(combined.lengthCM == length.lengthCM)
        #expect(combined.headCircumferenceCM == head.headCircumferenceCM)
    }

    @Test func rejectsEmptyAndInconsistentPositionWithSpecificErrors() {
        #expect(throws: MeasurementValues.ValidationError.empty) { try MeasurementValues() }
        #expect(throws: MeasurementValues.ValidationError.missingLengthPosition) {
            try MeasurementValues(lengthCM: 65.5)
        }
        #expect(throws: MeasurementValues.ValidationError.unexpectedLengthPosition) {
            try MeasurementValues(weightKG: 7.125, lengthPosition: .lying)
        }
    }

    @Test(arguments: [0.0, -1.0, Double.infinity, -Double.infinity, Double.nan])
    func rejectsInvalidValuesForEachMetric(value: Double) {
        #expect(throws: MeasurementValues.ValidationError.invalidValue(.weight)) {
            try MeasurementValues(weightKG: value)
        }
        #expect(throws: MeasurementValues.ValidationError.invalidValue(.lengthHeight)) {
            try MeasurementValues(lengthCM: value, lengthPosition: .lying)
        }
        #expect(throws: MeasurementValues.ValidationError.invalidValue(.headCircumference)) {
            try MeasurementValues(headCircumferenceCM: value)
        }
    }
}
