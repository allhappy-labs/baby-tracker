import Foundation
import BabyCore

extension HistoryDayTotals {
    /// A day with nothing to total: only running timers, or a filter that
    /// hides every tracker it had.
    static let none = HistoryDayTotals(feeds: 0, sleep: "", bottleML: 0, diapers: 0)

    /// The totals a day shows, as words, in header order: "8 feeds",
    /// "13h 20m sleep", "540 ml bottle", "6 diapers". VoiceOver reads them
    /// joined with commas; the PDF prints them joined with " · ".
    func phrases(volumeUnit: VolumeUnit) -> [String] {
        var phrases: [String] = []
        if feeds > 0 { phrases.append(L10n.fmt(feeds == 1 ? "%lld feed" : "%lld feeds", feeds)) }
        if !sleep.isEmpty { phrases.append(L10n.fmt("%@ sleep", sleep)) }
        if bottleML > 0 { phrases.append(L10n.fmt("%@ bottle", Volume(millilitres: bottleML).formatted(as: volumeUnit))) }
        if diapers > 0 { phrases.append(L10n.fmt(diapers == 1 ? "%lld diaper" : "%lld diapers", diapers)) }
        return phrases
    }
}
