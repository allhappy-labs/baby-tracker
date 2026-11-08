import Foundation
import BabyCore
import BabyStore
import BabyUI

/// One History entry as built: the row, when it happened (for ordering), and
/// its note, which the list does not show but the PDF does.
struct BuiltEntry: Equatable {
    let at: Date
    let row: HistoryListRow
    /// The entry's note; a session joins its parts' notes with " · ". Nil
    /// when there is none.
    let note: String?
}

/// One day as the store has it, before History titles it or adds running
/// timers. Only days with at least one entry the filter shows are built:
/// a non-zero total always comes with a row of its tracker.
struct BuiltDay: Equatable {
    let key: DayKey
    let totals: HistoryDayTotals
    /// Newest first; equal times fall back to the row id, so the order is
    /// stable from one load to the next.
    let entries: [BuiltEntry]
}

struct BuiltRange {
    /// The start of the first day built.
    let windowStart: Date
    let days: [DayKey: BuiltDay]
    /// Everything fetched, unfiltered — History's empty state asks whether
    /// anything exists at all.
    let items: [HistoryItem]
    let measurements: [MeasurementCheckIn]
}

/// Turns the store into History's days for any run of days and any filter.
/// The list builds its paging window with the user's filter; the PDF export
/// builds the chosen dates with every tracker. One builder, so the header
/// and the printout never disagree about a day.
@MainActor
struct HistoryDayBuilder {
    let store: BabyStore
    let babyID: UUID
    let volumeUnit: VolumeUnit
    let zone: String

    func build(from first: DayKey, through last: DayKey, filter: Set<Tracker>) throws -> BuiltRange {
        let windowStart = DayBucket.bounds(of: first, timeZoneID: zone).start
        let windowEnd = DayBucket.bounds(of: last, timeZoneID: zone).end
        func day(of date: Date) -> DayKey? {
            date >= windowStart && date < windowEnd ? DayBucket.key(for: date, timeZoneID: zone) : nil
        }

        // Session members start at or after their session's first part, so
        // a fetch from the window's start holds every member of every
        // session that starts inside it, a part after midnight included.
        let items = try store.entries(for: babyID, from: windowStart, to: .distantFuture)
        var feedsByID: [UUID: FeedEntry] = [:]
        var sleepsByID: [UUID: SleepEntry] = [:]
        var dated: [DayKey: [BuiltEntry]] = [:]
        var diaperCount: [DayKey: Int] = [:]
        for item in items {
            switch item {
            case .feed(let feed): feedsByID[feed.id] = feed
            case .sleep(let sleep): sleepsByID[sleep.id] = sleep
            case .diaper(let diaper):
                guard let key = day(of: diaper.startedAt) else { continue }
                diaperCount[key, default: 0] += 1
                if filter.contains(.diaper) {
                    dated[key, default: []].append(BuiltEntry(at: diaper.startedAt, row: diaperRow(diaper), note: Self.note([diaper.note])))
                }
            case .supplement(let med):
                guard filter.contains(.supplement), let key = day(of: med.startedAt) else { continue }
                dated[key, default: []].append(BuiltEntry(at: med.startedAt, row: medRow(med), note: Self.note([med.note])))
            case .immunization(let shot):
                guard filter.contains(.vaccine), let key = day(of: shot.givenAt) else { continue }
                dated[key, default: []].append(BuiltEntry(at: shot.givenAt, row: immunizationRow(shot), note: Self.note([shot.note])))
            case .pump(let pump):
                // A session still running is not a day's entry yet.
                guard filter.contains(.pump), let end = pump.endedAt, let key = day(of: pump.startedAt) else { continue }
                dated[key, default: []].append(BuiltEntry(at: pump.startedAt, row: pumpRow(pump, end: end), note: Self.note([pump.note])))
            }
        }

        // Feeds and sleeps come from the session projections Home and
        // Insights use, so a joined session is one row and one count, on the
        // day its first part started.
        var feedCount: [DayKey: Int] = [:]
        var bottleML: [DayKey: Double] = [:]
        for session in try store.feedingSessions(for: babyID, from: windowStart, to: windowEnd) {
            guard let key = day(of: session.startedAt) else { continue }
            let members = session.feedIDs.compactMap { feedsByID[$0] }
            guard !members.isEmpty else { continue }
            feedCount[key, default: 0] += 1
            bottleML[key, default: 0] += members.reduce(0) { $0 + $1.totalBottleVolumeML }
            if filter.contains(.feed) {
                dated[key, default: []].append(BuiltEntry(
                    at: session.startedAt, row: feedRow(session, members: members), note: Self.note(members.map(\.note))
                ))
            }
        }
        var sleepSeconds: [DayKey: Int] = [:]
        for session in try store.sleepSessions(for: babyID, from: windowStart, to: windowEnd) {
            let members = session.sleepIDs.compactMap { sleepsByID[$0] }
            guard let key = day(of: session.startedAt), let first = members.first else { continue }
            sleepSeconds[key, default: 0] += session.asleepSeconds
            if filter.contains(.sleep) {
                dated[key, default: []].append(BuiltEntry(
                    at: session.startedAt, row: sleepRow(session, first: first), note: Self.note(members.map(\.note))
                ))
            }
        }

        // H-R1: growth isn't a History entry in the store, so its check-ins
        // are merged in here by when they were measured.
        let measurements = try store.measurements(for: babyID)
        if filter.contains(.growth) {
            for check in measurements {
                guard let key = day(of: check.measuredAt) else { continue }
                dated[key, default: []].append(BuiltEntry(at: check.measuredAt, row: growthRow(check), note: nil))
            }
        }

        var days: [DayKey: BuiltDay] = [:]
        for (key, entries) in dated {
            days[key] = BuiltDay(
                key: key,
                totals: HistoryDayTotals(
                    feeds: filter.contains(.feed) ? feedCount[key, default: 0] : 0,
                    sleep: filter.contains(.sleep) ? sleepSeconds[key].map(ElapsedFormatter.total) ?? "" : "",
                    bottleML: filter.contains(.feed) ? bottleML[key, default: 0] : 0,
                    diapers: filter.contains(.diaper) ? diaperCount[key, default: 0] : 0
                ),
                entries: entries.sorted {
                    $0.at == $1.at ? $0.row.id.uuidString < $1.row.id.uuidString : $0.at > $1.at
                }
            )
        }
        return BuiltRange(windowStart: windowStart, days: days, items: items, measurements: measurements)
    }

    /// The non-blank notes, trimmed, joined; nil when none is left.
    static func note(_ notes: [String?]) -> String? {
        let kept = notes.compactMap { $0?.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty }
        return kept.isEmpty ? nil : kept.joined(separator: " · ")
    }

    // MARK: - Rows

    private func feedRow(_ session: FeedingSession, members: [FeedEntry]) -> HistoryListRow {
        let first = members[0]
        let components = members.flatMap(\.components)
        let chips = components.enumerated().map { chip($0.offset, $0.element) }
        // A single part opens its own editor, as solids on their own do;
        // solids added to a feed open with it, in the session sheet.
        let isSinglePart = members.count == 1 && (first.kind == .solids || first.components.count <= 1)
        return HistoryListRow(
            id: session.id,
            tracker: .feed,
            start: HistoryViewModel.clockTime(session.startedAt, zone: first.timeZoneID),
            end: session.endedAt > session.startedAt ? HistoryViewModel.clockTime(session.endedAt, zone: first.timeZoneID) : nil,
            chips: chips,
            // A defensive branch: a feed with no components has no chip.
            text: chips.isEmpty ? "—" : nil,
            isRunning: false,
            runningSince: nil,
            target: isSinglePart ? .entry(.feed(first.id)) : .session(.feed, rootID: session.id),
            deleteConfirmation: !isSinglePart
        )
    }

    func chip(_ index: Int, _ component: FeedComponent) -> HistoryChip {
        switch component {
        case let .breast(_, side, seconds, _, _):
            let letter = (side == .left ? BabyIcon.breastLeft : .breastRight).letter ?? ""
            return HistoryChip(id: index, icon: nil, text: letter + " " + HistoryViewModel.chipDuration(seconds))
        case let .bottle(_, volumeML, _, _, _, _):
            return HistoryChip(id: index, icon: .bottle, text: Volume(millilitres: volumeML).formatted(as: volumeUnit))
        case let .solids(_, foods, amount):
            // Foods and amount are the content of a solids feed. Only the
            // first food is capitalised: they are ordinary nouns.
            let parts = (foods.isEmpty ? [] : [foods.joined(separator: ", ").sentenceCased]) + [amount.displayName]
            return HistoryChip(id: index, icon: .solids, text: parts.joined(separator: " · "))
        }
    }

    private func sleepRow(_ session: SleepSession, first: SleepEntry) -> HistoryListRow {
        let wakes = session.sleepIDs.count - 1
        var text = ElapsedFormatter.total(session.asleepSeconds)
        if wakes > 0 { text += " · " + L10n.fmt(wakes == 1 ? "%lld wake" : "%lld wakes", wakes) }
        let isSinglePart = session.sleepIDs.count == 1
        return HistoryListRow(
            id: session.id,
            tracker: .sleep,
            start: HistoryViewModel.clockTime(session.startedAt, zone: first.timeZoneID),
            end: session.endedAt.flatMap { $0 > session.startedAt ? HistoryViewModel.clockTime($0, zone: first.timeZoneID) : nil },
            chips: [],
            text: text,
            isRunning: false,
            runningSince: nil,
            target: isSinglePart ? .entry(.sleep(first.id)) : .session(.sleep, rootID: session.id),
            deleteConfirmation: !isSinglePart
        )
    }

    private func diaperRow(_ diaper: DiaperEntry) -> HistoryListRow {
        singleRow(id: diaper.id, tracker: .diaper, at: diaper.startedAt, zone: diaper.timeZoneID,
                  text: diaper.contents.displayName, target: .diaper(diaper.id))
    }

    private func medRow(_ med: SupplementEntry) -> HistoryListRow {
        singleRow(id: med.id, tracker: .supplement, at: med.startedAt, zone: med.timeZoneID,
                  text: med.name, target: .supplement(med.id))
    }

    private func growthRow(_ check: MeasurementCheckIn) -> HistoryListRow {
        func number(_ value: Double) -> String { value.formatted(.number.precision(.significantDigits(1...6))) }
        let values = [
            check.weightKG.map { L10n.fmt("%@ kg", number($0)) },
            check.lengthCM.map { L10n.fmt("%@ cm", number($0)) },
            check.headCircumferenceCM.map { L10n.fmt("%@ cm", number($0)) },
        ].compactMap { $0 }
        return singleRow(id: check.id, tracker: .growth, at: check.measuredAt, zone: check.timeZoneID,
                         text: values.joined(separator: " · "), target: .measurement(check.id))
    }

    private func immunizationRow(_ shot: ImmunizationEntry) -> HistoryListRow {
        let text = shot.dose.map { L10n.fmt("%1$@ · dose %2$lld", shot.vaccine, $0) } ?? shot.vaccine
        return singleRow(id: shot.id, tracker: .vaccine, at: shot.givenAt, zone: shot.timeZoneID,
                         text: text, target: .immunization(shot.id))
    }

    /// "20m · 90 ml · Both"; "—" for a session with no amount. The same row
    /// in every baby's History: a pump belongs to the family.
    private func pumpRow(_ pump: PumpEntry, end: Date) -> HistoryListRow {
        let amount = pump.volumeML.map { Volume(millilitres: $0).formatted(as: volumeUnit) } ?? HistoryCopy.noAmount
        let text = [ElapsedFormatter.total(Int(end.timeIntervalSince(pump.startedAt))), amount, pump.side.displayName]
            .joined(separator: " · ")
        return HistoryListRow(id: pump.id, tracker: .pump,
                              start: HistoryViewModel.clockTime(pump.startedAt, zone: pump.timeZoneID),
                              end: HistoryViewModel.clockTime(end, zone: pump.timeZoneID), chips: [], text: text,
                              isRunning: false, runningSince: nil, target: .entry(.pump(pump.id)),
                              deleteConfirmation: false)
    }

    private func singleRow(id: UUID, tracker: Tracker, at date: Date, zone: String,
                           text: String, target: EntryRef) -> HistoryListRow {
        HistoryListRow(id: id, tracker: tracker, start: HistoryViewModel.clockTime(date, zone: zone), end: nil,
                       chips: [], text: text, isRunning: false, runningSince: nil, target: .entry(target),
                       deleteConfirmation: false)
    }
}
