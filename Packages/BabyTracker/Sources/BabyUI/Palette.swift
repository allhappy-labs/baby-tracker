import SwiftUI

/// Night Shift. Amber leads because it is the accent least hostile to
/// dark-adapted eyes, and this app's peak usage is at 3am.
public enum Palette {
    public static let feed = Color(red: 0.949, green: 0.690, blue: 0.380)    // #F2B061
    public static let sleep = Color(red: 0.541, green: 0.608, blue: 0.941)   // #8A9BF0
    public static let diaper = Color(red: 0.373, green: 0.827, blue: 0.769)  // #5FD3C4
    public static let ground = Color(red: 0.051, green: 0.071, blue: 0.141)  // #0D1224
    public static let primaryText = Color(red: 0.953, green: 0.961, blue: 0.992)
    public static let secondaryText = Color(red: 0.604, green: 0.643, blue: 0.788)
    public static let meds = Color(red: 0.77, green: 0.65, blue: 0.96)       // #C4A6F5
    public static let solids = Color(red: 0.91, green: 0.60, blue: 0.48)     // #E89A7B
    public static let growth = Color(red: 0.62, green: 0.82, blue: 0.48)     // #9FD27A
    public static let wet = Color(red: 0.50, green: 0.85, blue: 1.0)         // #7FD8FF
    public static let dirty = Color(red: 0.79, green: 0.64, blue: 0.48)      // #C9A27A
    /// Desaturated to the same degree as the three trackers rather than a pure
    /// red: on the near-black ground, full-saturation red blooms and is the
    /// one thing on screen that undoes dark adaptation.
    public static let destructive = Color(red: 0.937, green: 0.451, blue: 0.451) // #EF7373

    // Baby avatar tints, approved 2026-10-08.
    public static let avatarBoy1 = Color(red: 0.435, green: 0.698, blue: 1.000) // #6FB2FF
    public static let avatarBoy2 = Color(red: 0.310, green: 0.847, blue: 0.690) // #4FD8B0
    public static let avatarBoy3 = Color(red: 1.000, green: 0.847, blue: 0.302) // #FFD84D
    public static let avatarBoy4 = Color(red: 0.541, green: 0.518, blue: 1.000) // #8A84FF
    public static let avatarGirl1 = Color(red: 1.000, green: 0.525, blue: 0.722) // #FF86B8
    public static let avatarGirl2 = Color(red: 1.000, green: 0.702, blue: 0.361) // #FFB35C
    public static let avatarGirl3 = Color(red: 0.847, green: 0.608, blue: 1.000) // #D89BFF
    public static let avatarGirl4 = Color(red: 1.000, green: 0.420, blue: 0.420) // #FF6B6B
}

public enum Tracker: String, CaseIterable, Sendable {
    case feed, sleep, diaper, supplement, solids, growth

    public var color: Color {
        switch self {
        case .feed: Palette.feed
        case .sleep: Palette.sleep
        case .diaper: Palette.diaper
        case .supplement: Palette.meds
        case .solids: Palette.solids
        case .growth: Palette.growth
        }
    }

    /// This tracker's glyph in the app's own icon set. Every tracker renders
    /// through `BabyIconView(tracker.icon)` now; there is no SF Symbol left
    /// to fall back to.
    public var icon: BabyIcon {
        switch self {
        case .feed: .feed
        case .sleep: .sleep
        case .diaper: .diaper
        case .supplement: .meds
        case .solids: .solids
        case .growth: .growth
        }
    }

    public var title: String {
        switch self {
        case .feed: String(localized: "Feed", bundle: .module)
        case .sleep: String(localized: "Sleep", bundle: .module)
        case .diaper: String(localized: "Diaper", bundle: .module)
        case .supplement: String(localized: "Meds", bundle: .module)
        case .solids: String(localized: "Solids", bundle: .module)
        case .growth: String(localized: "Growth", bundle: .module)
        }
    }

    /// The original four, before `.solids` and `.growth` joined `Tracker` as
    /// the icon set's palette foundation. Only History's filter uses this
    /// list now: solids are filtered as feeds, and growth is not a History
    /// entry. On Home, Solids and Growth are reached from the More sheet.
    public static let loggable: [Tracker] = [.feed, .sleep, .diaper, .supplement]
}
