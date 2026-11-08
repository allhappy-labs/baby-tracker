import SwiftUI
import BabyCore

/// How a Home action looks wherever it is offered: the dock, the ··· sheet,
/// the settings list and the Quick Log widget.
public extension HomeActionKind {
    var icon: BabyIcon {
        switch self {
        case .breast: .feed
        case .bottle: .bottle
        case .sleep: .sleep
        case .diaper: .diaper
        case .solids: .solids
        case .meds: .meds
        case .growth: .growth
        case .vaccine: .vaccine
        case .pump: .pump
        }
    }

    var color: Color {
        switch self {
        case .breast, .bottle: Palette.feed
        case .sleep: Palette.sleep
        case .diaper: Palette.diaper
        case .solids: Palette.solids
        case .meds: Palette.meds
        case .growth: Palette.growth
        case .vaccine: Palette.vaccine
        case .pump: Palette.pump
        }
    }

    var title: String {
        switch self {
        case .breast: String(localized: "Breast", bundle: .module)
        case .bottle: String(localized: "Bottle", bundle: .module)
        case .sleep: Tracker.sleep.title
        case .diaper: Tracker.diaper.title
        case .solids: Tracker.solids.title
        case .meds: Tracker.supplement.title
        case .growth: Tracker.growth.title
        case .vaccine: Tracker.vaccine.title
        case .pump: Tracker.pump.title
        }
    }
}
