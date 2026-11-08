import Foundation
import SwiftData
import BabyCore

public extension BabyStore {
    /// Resolve a saved source link even after it falls outside History's visible days.
    func entry(id: UUID, for babyID: UUID) throws -> HistoryItem? {
        if let feed = try context.fetch(FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.id == id && $0.babyID == babyID && !$0.isTombstoned }
        )).first { return .feed(feed) }
        if let sleep = try context.fetch(FetchDescriptor<SleepEntry>(
            predicate: #Predicate { $0.id == id && $0.babyID == babyID && !$0.isTombstoned }
        )).first { return .sleep(sleep) }
        if let diaper = try context.fetch(FetchDescriptor<DiaperEntry>(
            predicate: #Predicate { $0.id == id && $0.babyID == babyID && !$0.isTombstoned }
        )).first { return .diaper(diaper) }
        if let med = try supplementRows(for: babyID).first(where: { $0.id == id }) { return .supplement(med) }
        if let shot = try immunizations(for: babyID).first(where: { $0.id == id }) { return .immunization(shot) }
        // A pump belongs to the family: found by id alone, for every baby.
        return try pump(id: id).map(HistoryItem.pump)
    }

    /// Fetched ascending and read from the end, not fetched descending and
    /// read from the front: SwiftData's sort is stable, so entries with an
    /// identical `startedAt` (reachable once backdated entries exist, e.g.
    /// via the Task 23 edit sheet) keep their original insertion order
    /// regardless of the requested `SortDescriptor` direction. Reading the
    /// tail of an ascending fetch means the most-recently-inserted entry
    /// wins on ties; reading the head of a descending fetch does not. No
    /// `fetchLimit` here — it would defeat the tie-break by truncating
    /// before we can read the tail.
    func lastFeed(for babyID: UUID) throws -> FeedEntry? {
        var descriptor = FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.babyID == babyID && !$0.isTombstoned }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .forward)]
        return try context.fetch(descriptor).last
    }

    func lastSleep(for babyID: UUID) throws -> SleepEntry? {
        var descriptor = FetchDescriptor<SleepEntry>(
            predicate: #Predicate { $0.babyID == babyID && !$0.isTombstoned }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .forward)]
        return try context.fetch(descriptor).last
    }

    func lastDiaper(for babyID: UUID) throws -> DiaperEntry? {
        var descriptor = FetchDescriptor<DiaperEntry>(
            predicate: #Predicate { $0.babyID == babyID && !$0.isTombstoned }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .forward)]
        return try context.fetch(descriptor).last
    }

    func homeSnapshot(for babyID: UUID) throws -> HomeSnapshot {
        let today = DayBucket.key(for: now, timeZoneID: TimeZone.current.identifier)
        let bounds = DayBucket.bounds(of: today, timeZoneID: TimeZone.current.identifier)
        let (start, end) = (bounds.start, bounds.end)

        let allFeeds = try sessionFeeds(for: babyID)
        let sessions = Self.projectSessions(allFeeds)
        let lastFeed = try lastFeed(for: babyID)
        let lastSession = sessions.first { $0.id == lastFeed?.effectiveFeedingSessionID }
        let feedsByID = Dictionary(uniqueKeysWithValues: allFeeds.map { ($0.id, $0) })
        let diapers = try context.fetch(FetchDescriptor<DiaperEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        ))

        let limits = breakLimits()
        let runningTimers = try activeTimers(for: babyID)
        let feedTimer = runningTimers.first { $0.kind == .feed }
        let sleepTimer = runningTimers.first { $0.kind == .sleep }
        let milkSessions = Self.projectSessions(allFeeds.filter { $0.kind != .solids })
        let lastMilk = milkSessions.max {
            $0.endedAt == $1.endedAt ? $0.id.uuidString < $1.id.uuidString : $0.endedAt < $1.endedAt
        }
        let feedBreak = feedTimer == nil ? lastMilk.flatMap { now.timeIntervalSince($0.endedAt) <= limits.feed ? $0 : nil } : nil
        let allSleeps = try sessionSleeps(for: babyID)
        let sleepSessions = Self.projectSleepSessions(allSleeps)
        let lastSleepEnd = allSleeps.compactMap(\.endedAt).max()
        let todaySleepSessions = sleepSessions.filter { $0.startedAt >= start && $0.startedAt < end }
        let todaySleepSpans = allSleeps.filter { ($0.endedAt ?? now) > start && $0.startedAt < end }
            .map { DateInterval(start: max($0.startedAt, start), end: min($0.endedAt ?? now, now)) }

        let runningFeedSession = feedTimer?.sessionID.flatMap { id in sessions.first { $0.id == id } }
        let exposedFeedIDs = Set([lastSession, feedBreak, runningFeedSession].compactMap { $0 }.flatMap(\.feedIDs))

        var snapshot = HomeSnapshot(
            lastFeed: lastFeed,
            lastFeedingSession: lastSession,
            lastSleep: try lastSleep(for: babyID),
            lastDiaper: try lastDiaper(for: babyID),
            runningTimers: runningTimers,
            todayFeedCount: sessions.filter { $0.startedAt >= start && $0.startedAt < end }.count,
            todayVolumeML: try measuredBottleSteps(for: babyID, from: start, to: end).reduce(0) { $0 + $1.volumeML },
            todaySleepSeconds: todaySleepSessions.reduce(0) { $0 + $1.asleepSeconds },
            todayDiaperCount: diapers.count,
            feedBreak: feedBreak,
            runningFeedSession: runningFeedSession,
            runningSleepSession: sleepTimer?.sessionID.flatMap { id in sleepSessions.first { $0.id == id } },
            awakeSince: lastSleepEnd,
            breakLimits: limits,
            todaySleepSpans: todaySleepSpans,
            todayFeedTimes: allFeeds.filter { $0.kind != .solids && $0.startedAt >= start && $0.startedAt < end }.map(\.startedAt),
            sessionFeedsByID: feedsByID.filter { exposedFeedIDs.contains($0.key) }
        )
        snapshot.runningPump = try runningPump()
        snapshot.todayPumpML = try pumps(from: start, to: end).filter { $0.endedAt != nil }.compactMap(\.volumeML).reduce(0, +)
        snapshot.lastPumpEndedAt = try lastFinishedPump()?.endedAt
        return snapshot
    }

    /// One local day of history, newest first, all kinds merged.
    func entries(for babyID: UUID, on day: DayKey) throws -> [HistoryItem] {
        let bounds = DayBucket.bounds(of: day, timeZoneID: TimeZone.current.identifier)
        let (start, end) = (bounds.start, bounds.end)

        let feeds = try context.fetch(FetchDescriptor<FeedEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        )).map(HistoryItem.feed)

        let sleeps = try context.fetch(FetchDescriptor<SleepEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        )).map(HistoryItem.sleep)

        let diapers = try context.fetch(FetchDescriptor<DiaperEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        )).map(HistoryItem.diaper)

        let supplements = try supplementRows(for: babyID).filter { $0.startedAt >= start && $0.startedAt < end }.map(HistoryItem.supplement)
        let shots = try immunizations(for: babyID).filter { $0.givenAt >= start && $0.givenAt < end }.map(HistoryItem.immunization)
        let pumps = try pumps(from: start, to: end).map(HistoryItem.pump)
        return (feeds + sleeps + diapers + supplements + shots + pumps).sorted { $0.startedAt > $1.startedAt }
    }
}

public extension BabyStore {
    /// Every entry started inside a half-open window, newest first, all kinds
    /// merged.
    ///
    /// The per-day query is the right shape for one day of History. Asking it
    /// thirty times to draw a month of trend is ninety fetches for one screen,
    /// so the window is fetched once here and bucketed by its caller.
    func entries(for babyID: UUID, from start: Date, to end: Date) throws -> [HistoryItem] {
        let feeds = try context.fetch(FetchDescriptor<FeedEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        )).map(HistoryItem.feed)

        let sleeps = try context.fetch(FetchDescriptor<SleepEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        )).map(HistoryItem.sleep)

        let diapers = try context.fetch(FetchDescriptor<DiaperEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        )).map(HistoryItem.diaper)

        let supplements = try supplementRows(for: babyID).filter { $0.startedAt >= start && $0.startedAt < end }.map(HistoryItem.supplement)
        let shots = try immunizations(for: babyID).filter { $0.givenAt >= start && $0.givenAt < end }.map(HistoryItem.immunization)
        let pumps = try pumps(from: start, to: end).map(HistoryItem.pump)
        return (feeds + sleeps + diapers + supplements + shots + pumps).sorted { $0.startedAt > $1.startedAt }
    }
}
