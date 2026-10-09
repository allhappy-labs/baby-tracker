/// Raw recorded values in kilograms and centimeters, with no reference adjustment.
/// A check-in is nonempty and retains the method used for its length/height reading.
public struct MeasurementValues: Sendable, Equatable {
    public enum ValidationError: Error, Sendable, Equatable {
        case empty
        case invalidValue(GrowthMetric)
        case missingLengthPosition
        case unexpectedLengthPosition
    }

    public let weightKG: Double?
    public let lengthCM: Double?
    public let headCircumferenceCM: Double?
    public let lengthPosition: LengthPosition?

    public init(weightKG: Double? = nil, lengthCM: Double? = nil,
                headCircumferenceCM: Double? = nil, lengthPosition: LengthPosition? = nil) throws {
        guard weightKG != nil || lengthCM != nil || headCircumferenceCM != nil else {
            throw ValidationError.empty
        }
        for (metric, value) in [(GrowthMetric.weight, weightKG), (.lengthHeight, lengthCM),
                                (.headCircumference, headCircumferenceCM)] {
            if let value, !value.isFinite || value <= 0 { throw ValidationError.invalidValue(metric) }
        }
        if lengthCM != nil && lengthPosition == nil { throw ValidationError.missingLengthPosition }
        if lengthCM == nil && lengthPosition != nil { throw ValidationError.unexpectedLengthPosition }
        self.weightKG = weightKG
        self.lengthCM = lengthCM
        self.headCircumferenceCM = headCircumferenceCM
        self.lengthPosition = lengthPosition
    }
}
