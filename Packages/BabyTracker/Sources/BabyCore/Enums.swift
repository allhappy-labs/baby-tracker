import Foundation

public enum FeedKind: String, Codable, Sendable, CaseIterable {
    case breast, bottle, solids
}

public enum BreastSide: String, Codable, Sendable, CaseIterable {
    case left, right

    public var opposite: BreastSide {
        self == .left ? .right : .left
    }

    /// How the side is written on screen. Raw values are lower-case storage
    /// keys; rendering them straight put "left" beside properly capitalised
    /// siblings like "Dirty" in the same dot-separated line.
    public var displayName: String {
        switch self {
        case .left: String(localized: "Left", bundle: .module)
        case .right: String(localized: "Right", bundle: .module)
        }
    }

}

public enum BottleContents: String, Codable, Sendable, CaseIterable {
    case breastMilk, formula, mixed

    /// `breastMilk` is two words to a reader, and `.capitalized` on the raw
    /// value would render it "Breastmilk".
    public var displayName: String {
        switch self {
        case .breastMilk: String(localized: "Breast milk", bundle: .module)
        case .formula: String(localized: "Formula", bundle: .module)
        case .mixed: String(localized: "Mixed", bundle: .module)
        }
    }
}

public enum SolidsAmount: String, Codable, Sendable, CaseIterable {
    case taste, some, lots

    /// How much went in, written the way a parent would say it. Raw values are
    /// lower-case storage keys and must not reach the screen.
    public var displayName: String {
        switch self {
        case .taste: String(localized: "Taste", bundle: .module)
        case .some: String(localized: "Some", bundle: .module)
        case .lots: String(localized: "Lots", bundle: .module)
        }
    }
}

public enum DiaperContents: String, Codable, Sendable, CaseIterable {
    case wet, dirty, mixed, dry

    /// The three offered in the two-tap fast path. `.dry` is reachable only
    /// from the edit sheet.
    public static var fastPath: [DiaperContents] { [.wet, .dirty, .mixed] }

    public var displayName: String {
        switch self {
        case .wet: String(localized: "Wet", bundle: .module)
        case .dirty: String(localized: "Dirty", bundle: .module)
        case .mixed: String(localized: "Mixed", bundle: .module)
        case .dry: String(localized: "Dry", bundle: .module)
        }
    }
}

public enum SleepKind: String, Codable, Sendable, CaseIterable {
    case nap, night
}

public enum TimerKind: String, Codable, Sendable, CaseIterable {
    case feed, sleep
}
