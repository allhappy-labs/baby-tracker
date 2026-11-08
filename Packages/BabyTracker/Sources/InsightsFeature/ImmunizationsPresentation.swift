import Foundation
import BabyCore
import BabyStore

/// One vaccination as the card and the screen read it.
struct ImmunizationShot: Equatable, Sendable {
    let id: UUID
    let vaccine: String
    let dose: Int?
    let givenAt: Date
    let timeZoneID: String
}

/// The Immunizations card and screen, as text. Pure: the locale is an input,
/// so tests pin it.
struct ImmunizationsPresentation: Equatable {
    struct Row: Equatable, Identifiable {
        let id: UUID
        let text: String
    }
    struct Section: Equatable, Identifiable {
        var id: String { vaccine }
        let vaccine: String
        let rows: [Row]
    }

    let count: Int
    let latestText: String?
    let sections: [Section]
    var isEmpty: Bool { count == 0 }

    static let empty = ImmunizationsPresentation(shots: [], birthDay: nil)

    init(shots: [ImmunizationShot], birthDay: LocalDate?, locale: Locale = .current) {
        let newestFirst = shots.sorted { $0.givenAt > $1.givenAt }
        count = newestFirst.count
        latestText = newestFirst.first.map { shot in
            let date = shot.givenAt.formatted(Self.style(shot, locale).day().month(.abbreviated))
            let name = shot.dose.map { L10n.fmt("%1$@ · dose %2$lld", shot.vaccine, $0) } ?? shot.vaccine
            return "\(name) · \(date)"
        }
        // One section per vaccine, in the order of its newest shot; the heading
        // is the spelling of its earliest shot, as the picker shows it.
        var order: [String] = []
        var grouped: [String: [ImmunizationShot]] = [:]
        for shot in newestFirst {
            let key = BabyStore.vaccineNameKey(shot.vaccine)
            if grouped[key] == nil { order.append(key) }
            grouped[key, default: []].append(shot)
        }
        sections = order.map { key in
            let oldestFirst = Array((grouped[key] ?? []).reversed())
            return Section(vaccine: oldestFirst.first?.vaccine ?? "", rows: oldestFirst.map { shot in
                Row(id: shot.id, text: Self.rowText(shot, birthDay: birthDay, locale: locale))
            })
        }
    }

    private static func style(_ shot: ImmunizationShot, _ locale: Locale) -> Date.FormatStyle {
        Date.FormatStyle(locale: locale, timeZone: TimeZone(identifier: shot.timeZoneID) ?? .current)
    }

    private static func rowText(_ shot: ImmunizationShot, birthDay: LocalDate?, locale: Locale) -> String {
        var parts: [String] = []
        if let dose = shot.dose { parts.append(L10n.fmt("Dose %lld", dose)) }
        parts.append(shot.givenAt.formatted(style(shot, locale).day().month(.abbreviated).year()))
        if let age = age(of: shot, birthDay: birthDay, locale: locale) { parts.append(age) }
        return parts.joined(separator: " · ")
    }

    /// Growth's short age: days under two weeks, weeks under thirteen, months
    /// under two years, then years. Nil before the birth day.
    private static func age(of shot: ImmunizationShot, birthDay: LocalDate?, locale: Locale) -> String? {
        guard let birthDay, let zone = TimeZone(identifier: shot.timeZoneID),
              let givenDay = try? LocalDate(displaying: shot.givenAt, in: zone), givenDay >= birthDay else { return nil }
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "UTC")!
        calendar.locale = locale
        guard let from = date(birthDay, calendar), let to = date(givenDay, calendar) else { return nil }
        let days = calendar.dateComponents([.day], from: from, to: to).day ?? 0
        let months = calendar.dateComponents([.month], from: from, to: to).month ?? 0
        let formatter = DateComponentsFormatter()
        formatter.unitsStyle = .abbreviated
        formatter.maximumUnitCount = 1
        formatter.calendar = calendar
        formatter.allowedUnits = days < 14 ? [.day] : days < 91 ? [.weekOfMonth] : months < 24 ? [.month] : [.year]
        return formatter.string(from: from, to: to)
    }

    /// Midnight UTC of a calendar date ("2026-09-03").
    private static func date(_ day: LocalDate, _ calendar: Calendar) -> Date? {
        let parts = day.iso8601.split(separator: "-").compactMap { Int($0) }
        guard parts.count == 3 else { return nil }
        return calendar.date(from: DateComponents(year: parts[0], month: parts[1], day: parts[2]))
    }
}
