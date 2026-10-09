/// Stable storage and wire values. User-facing labels belong to presentation.
public enum GrowthMetric: String, Codable, Sendable, CaseIterable {
    case weight, lengthHeight, headCircumference
}

/// The selected growth reference, never inferred for an existing child.
public enum GrowthReferenceSex: String, Codable, Sendable, CaseIterable {
    case girls, boys
}

/// Retains how the original length/height value was measured.
public enum LengthPosition: String, Codable, Sendable, CaseIterable {
    case lying, standing
}
