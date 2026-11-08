import Foundation
import BabyCore

public extension LogLink {
    /// The panel a Quick Log widget tile's link opens, or nil for Home alone.
    ///
    /// `babies` are the babies still on this phone — a widget can go on
    /// drawing one deleted since, and its link must not open a panel that
    /// would log for whichever baby the app falls back to. `isBusy`: a sheet
    /// or an alert is already up, and a tap outside the app never replaces
    /// what a caregiver was in the middle of.
    func panel(babies: [UUID], canLog: Bool, isBusy: Bool) -> LogSheet? {
        guard babies.contains(babyID), canLog, !isBusy else { return nil }
        switch kind {
        case .bottle: return .bottle
        case .diaper: return .diaper
        case .solids: return .solids
        case .meds: return .meds
        case .growth: return .measurement
        case .vaccine: return .immunization
        case .pump: return .pump
        // Both log from the widget itself; a link for them only opens Home.
        case .breast, .sleep: return nil
        }
    }
}
