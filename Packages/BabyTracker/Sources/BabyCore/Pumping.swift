import Foundation

/// Which side a pumping session drew from. One timer and one amount for all
/// three.
public enum PumpSide: String, Codable, Sendable, CaseIterable {
    case left, right, both

    public var displayName: String {
        switch self {
        case .left: String(localized: "Left", bundle: .module)
        case .right: String(localized: "Right", bundle: .module)
        case .both: String(localized: "Both", bundle: .module)
        }
    }
}

/// The server's pump rules, so the phone never sends what it would refuse.
public enum PumpRules {
    /// The longest session the server accepts. A pump stopped later than
    /// this ends here.
    public static let maxSeconds: TimeInterval = 6 * 3600
    public static let maxVolumeML: Double = 1000
    /// How far ahead of the server's clock a start or end may be.
    public static let clockGrace: TimeInterval = 60

    /// Two decimals, the most the server takes. 3 oz is 88.72 ml.
    public static func roundedVolume(_ ml: Double) -> Double {
        (ml * 100).rounded() / 100
    }

    public static func isValidVolume(_ ml: Double?) -> Bool {
        guard let ml else { return true }
        return ml.isFinite && ml >= 0 && ml <= maxVolumeML
    }
}

public enum PumpCopy {
    /// "Pumping · Both": the strip and the Lock Screen card.
    public static func caption(_ side: PumpSide) -> String {
        String(format: String(localized: "Pumping · %@", bundle: .module), locale: .current, side.displayName)
    }
}
