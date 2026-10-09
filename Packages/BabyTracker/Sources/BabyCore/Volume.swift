import Foundation

public enum VolumeUnit: String, Codable, Sendable, CaseIterable {
    case millilitres, ounces

    /// What a person reads. `rawValue` is a storage key — "millilitres" is
    /// lowercase because it is a wire format, and must never reach a label.
    public var displayName: String {
        switch self {
        case .millilitres: String(localized: "Millilitres", bundle: .module)
        case .ounces: String(localized: "Ounces", bundle: .module)
        }
    }

    public var shortName: String {
        switch self {
        case .millilitres: "ml"
        case .ounces: "oz"
        }
    }
}

/// Millilitres are canonical everywhere. Ounces exist only at the display edge.
public struct Volume: Codable, Sendable, Equatable {
    private static let mlPerOunce = 29.5735295625

    public let millilitres: Double

    public init(millilitres: Double) {
        self.millilitres = millilitres
    }

    public init(ounces: Double) {
        self.millilitres = ounces * Self.mlPerOunce
    }

    public var ounces: Double {
        millilitres / Self.mlPerOunce
    }

    public func formatted(as unit: VolumeUnit) -> String {
        let value = unit == .millilitres ? millilitres : ounces
        let rounded = (value * 10).rounded() / 10
        let formatter = NumberFormatter()
        formatter.numberStyle = .decimal
        formatter.minimumFractionDigits = 0
        formatter.maximumFractionDigits = 1
        let text = formatter.string(from: NSNumber(value: rounded)) ?? String(rounded)
        return "\(text) \(unit.shortName)"
    }
}
