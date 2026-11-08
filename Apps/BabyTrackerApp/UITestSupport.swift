import Foundation
import SwiftData
import BabyCore
import BabyStore
import BabyUI

/// Launch-argument plumbing for XCUITest.
///
/// The whole file is behind `#if DEBUG`, which needs no pbxproj support: the app
/// target's Debug configuration inherits `SWIFT_ACTIVE_COMPILATION_CONDITIONS =
/// "DEBUG $(inherited)"` and Release does not define it, so none of this reaches a
/// release build. That matters more than the `-uiTest…` argument gate: `emptyStore`
/// deletes `ChangeLogEntry` along with everything else, and without the change log a
/// future sync server cannot reconstruct any of it. Low exploitability — a
/// SpringBoard-launched app gets no argv — but the blast radius is total and the
/// guard is free.
#if DEBUG
@MainActor
enum UITestSupport {
    /// The banner is otherwise only reachable through a store failure, which
    /// cannot be provoked in the simulator — so the one thing that could not be
    /// verified at runtime stays unverified forever. This seeds it directly, so
    /// `testErrorBannerDismissIsReachableWithoutTheLabelAbsorbingIt` can prove
    /// the dismiss control is its own actionable element.
    static func seedErrorBannerIfRequested(_ reporter: ErrorReporter) {
        guard ProcessInfo.processInfo.arguments.contains("-uiTestShowErrorBanner") else { return }
        reporter.report(
            ErrorBannerCopy.saveFailed(what: ErrorSubject.diaperChange),
            error: SeededFailure()
        )
    }

    private struct SeededFailure: Error {}

    /// Empties the App Group database through `ModelContext`, which is the real
    /// persistence API — the store file itself is never touched.
    /// Returns the app to a clean, local-only state.
    ///
    /// Records alone are not enough. The realm and the family write permission
    /// are durable too, so a run that left the app in a shared realm as a
    /// read-only caregiver would hand that state to every later test in the
    /// same simulator container — and they would fail with no logging controls
    /// and no obvious cause. A reset means local-only, writable, empty.
    static func emptyStore(_ store: BabyStore) {
        let context = store.context

        // Permissions first: while a viewer permission stands, nothing else
        // can be written.
        try? context.delete(model: RealmWritePermission.self)
        try? context.delete(model: AccountAccessState.self)
        try? context.save()

        // Then back to the permanent local realm, so records seeded next land
        // where every other test expects to find them.
        try? store.recoverIncompleteTransition()
        if store.activeRealm != .local {
            let checkpoint = UUID()
            try? store.beginTransition(to: .local, checkpointID: checkpoint)
            try? store.commitTransition(checkpointID: checkpoint)
        }

        try? context.delete(model: ChangeLogEntry.self)
        try? context.delete(model: ActiveTimer.self)
        try? context.delete(model: FeedEntry.self)
        try? context.delete(model: SleepEntry.self)
        try? context.delete(model: DiaperEntry.self)
        // Vaccine names are the whole realm's: a shot left behind would show in
        // the next test's picker.
        try? context.delete(model: ImmunizationEntry.self)
        try? context.delete(model: PumpEntry.self)
        try? context.delete(model: Baby.self)
        try? context.delete(model: SyncOutboxEntry.self)
        try? context.delete(model: CanonicalEntityBase.self)
        try? context.delete(model: SyncCursorState.self)
        try? context.delete(model: CaregiverAttribution.self)
        try? context.delete(model: ImportCheckpoint.self)
        try? context.delete(model: FencedLease.self)
        try? context.save()
    }

    /// Feeds, sleeps and diapers across today and yesterday, so History has two
    /// day sections and Insights has a non-empty seven-day window to compare
    /// against. Runtime actions go through the public `BabyStore` verbs; the
    /// historical breast fixture is inserted directly because a real immediate
    /// start/stop is now correctly rejected as an empty feed.
    ///
    /// Today's timestamps are interpolated between local midnight and `now`
    /// rather than fixed clock hours, so they are always in the past and always
    /// inside today's `DayBucket` no matter what time the suite runs.
    /// Puts the app into the read-only-caregiver state without a server.
    ///
    /// Joining a family as a viewer needs an account, an endpoint and a live
    /// server, none of which a simulator UI test has. What the screens actually
    /// depend on is narrower than that: an active shared realm whose durable
    /// write permission says `viewer`. Seeding exactly that renders the real
    /// state through the real code path — the gate under test is the store's,
    /// not the transport's.
    static func seedReadOnlyFamily(_ store: BabyStore, babyName: String) -> UUID? {
        let realm = RealmID.shared(
            canonicalOrigin: "https://sync.example.test",
            serverInstanceID: UUID(uuidString: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!,
            familyID: UUID(uuidString: "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
        )
        do {
            if store.activeRealm != realm {
                let checkpoint = UUID()
                try store.beginTransition(to: realm, checkpointID: checkpoint)
                try store.commitTransition(checkpointID: checkpoint)
            }
            // Deliberately reset to a writable role first. The permission row
            // survives `-uiTestResetStore`, which empties records rather than
            // deleting the database, so a second run would otherwise be
            // refused the seed baby by the very gate it is here to set up.
            try store.setWritePermission(.caregiver, for: realm)
            let baby = try store.babies().first ?? store.addBaby(name: babyName, birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
            // History's fixture, written while the role still allows it, so
            // the viewer has rows to read (and not to tap).
            if ProcessInfo.processInfo.arguments.contains(historySessionsArgument) {
                seedHistorySessions(store, babyID: baby.id)
            }
            // Recorded last: from here the store refuses the write, which is
            // the whole point.
            try store.setWritePermission(.viewer, for: realm)
            return baby.id
        } catch {
            AppLog.store.error("read-only UI fixture failed: \(error.localizedDescription, privacy: .private)")
            return nil
        }
    }

    static func seedFixture(_ store: BabyStore, babyID: UUID) {
        guard (try? store.lastDiaper(for: babyID)) == nil else { return }

        let now = store.now
        let zone = TimeZone.current.identifier
        let calendar = Calendar.current

        let todayStart = DayBucket.bounds(
            of: DayBucket.key(for: now, timeZoneID: zone), timeZoneID: zone
        ).start
        let span = now.timeIntervalSince(todayStart)
        func today(_ fraction: Double) -> Date {
            todayStart.addingTimeInterval(span * fraction)
        }

        let yesterdayStart = calendar.date(byAdding: .day, value: -1, to: todayStart) ?? todayStart
        func yesterday(_ hour: Double) -> Date {
            yesterdayStart.addingTimeInterval(hour * 3600)
        }

        // MARK: Yesterday — a full-looking day.
        backdatedBreastFeed(store, babyID: babyID, at: yesterday(7.5), seconds: 17 * 60, side: .left)
        try? store.logBottle(volumeML: 120, contents: .formula, for: babyID, at: yesterday(11))
        backdatedBreastFeed(store, babyID: babyID, at: yesterday(15), seconds: 22 * 60, side: .right)
        try? store.logBottle(volumeML: 150, contents: .breastMilk, for: babyID, at: yesterday(20))

        try? store.logSleep(from: yesterday(9), to: yesterday(10.5), for: babyID)
        try? store.logSleep(from: yesterday(13), to: yesterday(14.25), for: babyID)
        try? store.logSleep(from: yesterday(21), to: yesterday(23.75), for: babyID)

        try? store.logDiaper(.wet, for: babyID, at: yesterday(8))
        try? store.logDiaper(.dirty, for: babyID, at: yesterday(12.5))
        try? store.logDiaper(.mixed, for: babyID, at: yesterday(18))

        // MARK: Today — deliberately lighter, so Insights has a real delta.
        backdatedBreastFeed(store, babyID: babyID, at: today(0.2), seconds: 14 * 60, side: .left)
        try? store.logBottle(volumeML: 90, contents: .mixed, for: babyID, at: today(0.55))

        try? store.logSleep(from: today(0.3), to: today(0.45), for: babyID)

        try? store.logDiaper(.wet, for: babyID, at: today(0.35))
        try? store.logDiaper(.dirty, for: babyID, at: today(0.85))

        if ProcessInfo.processInfo.arguments.contains("-uiTestHomeCompositeFeed") {
            let feed = FeedEntry(babyID: babyID, kind: .bottle, startedAt: today(0.90), timeZoneID: zone)
            feed.endedAt = today(0.96)
            feed.components = [
                .bottle(id: UUID(), volumeML: 20, contents: .breastMilk, seconds: 502,
                        startedAt: today(0.90), endedAt: today(0.92)),
                .bottle(id: UUID(), volumeML: 30, contents: .breastMilk, seconds: 1_904,
                        startedAt: today(0.93), endedAt: today(0.96))
            ]
            store.context.insert(feed)
            try? store.context.save()
        }
        if ProcessInfo.processInfo.arguments.contains("-uiTestHomeMixedFeed") {
            let feed = FeedEntry(babyID: babyID, kind: .breast, startedAt: today(0.88), timeZoneID: zone)
            feed.endedAt = today(0.98)
            feed.components = [
                .breast(id: UUID(), side: .right, seconds: 423,
                        startedAt: today(0.88), endedAt: today(0.90)),
                .breast(id: UUID(), side: .left, seconds: 616,
                        startedAt: today(0.91), endedAt: today(0.93)),
                .bottle(id: UUID(), volumeML: 40, contents: .breastMilk, seconds: 310,
                        startedAt: today(0.94), endedAt: today(0.95)),
                .bottle(id: UUID(), volumeML: 40, contents: .formula, seconds: 385,
                        startedAt: today(0.96), endedAt: today(0.98))
            ]
            store.context.insert(feed)
            try? store.context.save()
        }
        if ProcessInfo.processInfo.arguments.contains("-uiTestHomeLinkedMixedSession") {
            // Each part ends when its own length says, laid back from
            // 19:30 yesterday with real gaps between them (21m, 18m, 14m).
            // Ends placed at day fractions instead disagreed with the
            // lengths, and the session sheet's gaps and the session's end,
            // which go by the recorded ends, read wrong. Yesterday, at a
            // fixed time, so the session lists under the same day whenever
            // the suite runs: laid back from today(0.98) it moved to
            // Yesterday for a launch before about 01:24. After the
            // fixture's 15:00 feed, before its 20:00 bottle.
            func part(_ kind: FeedKind, endingAt end: Date, seconds: Int,
                      _ component: (Date, Date) -> FeedComponent) -> FeedEntry {
                let start = end.addingTimeInterval(-TimeInterval(seconds))
                let feed = FeedEntry(babyID: babyID, kind: kind, startedAt: start, timeZoneID: zone)
                feed.endedAt = end
                feed.components = [component(start, end)]
                return feed
            }
            let formula = part(.bottle, endingAt: yesterday(19.5), seconds: 385) {
                .bottle(id: UUID(), volumeML: 40, contents: .formula, seconds: 385, startedAt: $0, endedAt: $1)
            }
            let milk = part(.bottle, endingAt: formula.startedAt.addingTimeInterval(-14 * 60), seconds: 310) {
                .bottle(id: UUID(), volumeML: 40, contents: .breastMilk, seconds: 310, startedAt: $0, endedAt: $1)
            }
            let left = part(.breast, endingAt: milk.startedAt.addingTimeInterval(-18 * 60), seconds: 616) {
                .breast(id: UUID(), side: .left, seconds: 616, startedAt: $0, endedAt: $1)
            }
            let right = part(.breast, endingAt: left.startedAt.addingTimeInterval(-21 * 60), seconds: 423) {
                .breast(id: UUID(), side: .right, seconds: 423, startedAt: $0, endedAt: $1)
            }

            for feed in [right, left, milk, formula] { store.context.insert(feed) }
            do {
                try store.context.save()
                try store.combineFeeds([right.id, left.id, milk.id, formula.id], for: babyID)
            } catch {
                AppLog.store.error("mixed session UI fixture failed: \(error.localizedDescription, privacy: .private)")
            }
        }
        if ProcessInfo.processInfo.arguments.contains("-uiTestRunningSleep") {
            try? store.startSleep(for: babyID)
        }
        if ProcessInfo.processInfo.arguments.contains("-uiTestRunningFeed") {
            try? store.startPart(.breast(.left), for: babyID)
        }
        if ProcessInfo.processInfo.arguments.contains("-uiTestRunningBottle") {
            try? store.startPart(.bottle(.formula), for: babyID)
        }
        // Two finished sessions earlier today: one measured, one skipped.
        if ProcessInfo.processInfo.arguments.contains("-uiTestSeedPumps") {
            _ = try? store.logPump(side: .left, from: now.addingTimeInterval(-3 * 3600), to: now.addingTimeInterval(-3 * 3600 + 900),
                                   volumeML: nil, note: nil)
            _ = try? store.logPump(side: .both, from: now.addingTimeInterval(-3600), to: now.addingTimeInterval(-2400),
                                   volumeML: 90, note: nil)
        }
        if ProcessInfo.processInfo.arguments.contains("-uiTestRunningPump") {
            _ = try? store.startPump(side: .both)
        }
        // Three saved meds, so the More sheet's row has enough chips to
        // scroll — screenshot fixture only, no app behaviour depends on it.
        if ProcessInfo.processInfo.arguments.contains("-uiTestSeedMeds") {
            try? store.logSupplement(name: "Vitamin D", for: babyID)
            try? store.logSupplement(name: "Iron", for: babyID)
            try? store.logSupplement(name: "Probiotic", for: babyID)
        }
        // A Left part of 9:02 that ended 40 s ago: the feed is in its break.
        if ProcessInfo.processInfo.arguments.contains("-uiTestFeedBreak") {
            _ = try? store.logBreast(side: .left, from: now.addingTimeInterval(-582),
                                     to: now.addingTimeInterval(-40), for: babyID)
        }
        // A stopped Left part, then a running Right part that joined it.
        if ProcessInfo.processInfo.arguments.contains("-uiTestJoinedFeed") {
            _ = try? store.logBreast(side: .left, from: now.addingTimeInterval(-722),
                                     to: now.addingTimeInterval(-180), for: babyID)
            _ = try? store.startPart(.breast(.right), for: babyID)
        }
    }

    static let historySessionsArgument = "-uiTestHistorySessions"

    /// A booklet of four shots for a baby five months old: two vaccines, two
    /// doses each, one with a note (`-uiTestSeedVaccines`).
    static func seedVaccines(_ store: BabyStore, babyID: UUID) {
        let now = store.now
        let day = 86_400.0
        if let baby = try? store.babies().first(where: { $0.id == babyID }) {
            try? store.updateBabyAndGrowthProfile(
                baby, name: baby.name, birthDate: now.addingTimeInterval(-150 * day),
                referenceSex: .girls, expectedDueDate: nil
            )
        }
        try? store.logImmunization(vaccine: "Infanrix hexa", dose: 1, for: babyID, at: now.addingTimeInterval(-90 * day))
        try? store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: babyID, at: now.addingTimeInterval(-90 * day + 300))
        try? store.logImmunization(vaccine: "Infanrix hexa", dose: 2, for: babyID, at: now.addingTimeInterval(-30 * day))
        try? store.logImmunization(vaccine: "Prevenar 13", dose: 2, for: babyID, at: now.addingTimeInterval(-30 * day + 300),
                                   note: "Fussy that evening")
    }

    /// History's redesign fixture (`-uiTestHistorySessions`): every kind of
    /// row and both kinds of session, at fixed clock times on the two days
    /// before today, so neither the rows nor their day depend on when the
    /// suite runs, and every session ended long before launch (ruling H-R18:
    /// Add part is off for a session that ended under a minute ago).
    ///
    /// Yesterday, newest first: solids at 17:30, growth at 16:00, a sleep of
    /// two parts with one wake (13:00–14:10, 14:25–15:30), a wet diaper at
    /// 12:00, a feed of Left, Right and a 60 ml bottle (10:23–10:35,
    /// 10:38–10:53, the bottle at 11:05), and Vitamin D at 09:00. The day
    /// before: a feed that crosses midnight, Left 23:45–23:57 and Right
    /// 00:05–00:15, listed once on the day it began.
    ///
    /// Everything goes through the store's own verbs, so the entries land in
    /// whichever realm is active — the read-only family seeds this before it
    /// turns the caregiver into a viewer. The baby is made ten days old
    /// first: a growth check-in may not predate the birth, and ten days is
    /// still inside History's first page, so no older page is loading.
    static func seedHistorySessions(_ store: BabyStore, babyID: UUID) {
        // Once per store: a launch without a reset keeps what is there.
        guard (try? store.measurements(for: babyID))?.isEmpty ?? true else { return }
        let calendar = Calendar.current
        let todayStart = calendar.startOfDay(for: store.now)
        func at(daysBack: Int, _ hour: Int, _ minute: Int) -> Date {
            let day = calendar.date(byAdding: .day, value: -daysBack, to: todayStart) ?? todayStart
            return calendar.date(bySettingHour: hour, minute: minute, second: 0, of: day) ?? day
        }
        do {
            if let baby = try store.babies().first(where: { $0.id == babyID }),
               baby.birthDate > at(daysBack: 10, 0, 0) {
                try store.updateBabyAndGrowthProfile(
                    baby, name: baby.name, birthDate: at(daysBack: 10, 8, 0),
                    referenceSex: .girls, expectedDueDate: nil
                )
            }

            // Two days back into yesterday: one feed across midnight.
            let lateLeft = try store.logBreast(side: .left, from: at(daysBack: 2, 23, 45),
                                               to: at(daysBack: 2, 23, 57), for: babyID)
            let earlyRight = try store.logBreast(side: .right, from: at(daysBack: 1, 0, 5),
                                                 to: at(daysBack: 1, 0, 15), for: babyID)
            joinFeeds(store, [lateLeft, earlyRight], babyID: babyID)

            try store.logSupplement(name: "Vitamin D", for: babyID, at: at(daysBack: 1, 9, 0))

            let left = try store.logBreast(side: .left, from: at(daysBack: 1, 10, 23),
                                           to: at(daysBack: 1, 10, 35), for: babyID)
            let right = try store.logBreast(side: .right, from: at(daysBack: 1, 10, 38),
                                            to: at(daysBack: 1, 10, 53), for: babyID)
            let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: babyID,
                                             at: at(daysBack: 1, 11, 5))
            joinFeeds(store, [left, right, bottle], babyID: babyID)

            try store.logDiaper(.wet, for: babyID, at: at(daysBack: 1, 12, 0))

            let nap = try store.logSleep(from: at(daysBack: 1, 13, 0), to: at(daysBack: 1, 14, 10), for: babyID)
            let resettled = try store.logSleep(from: at(daysBack: 1, 14, 25), to: at(daysBack: 1, 15, 30), for: babyID)
            if nap.effectiveSleepSessionID != resettled.effectiveSleepSessionID {
                try store.combineSleeps([nap.id, resettled.id], for: babyID)
            }

            try store.logMeasurement(
                for: babyID, at: at(daysBack: 1, 16, 0), timeZoneID: TimeZone.current.identifier,
                values: MeasurementValues(weightKG: 4.2)
            )
            try store.logSolids(foods: ["Pear"], amount: .some, for: babyID, at: at(daysBack: 1, 17, 30))
        } catch {
            AppLog.store.error("history sessions UI fixture failed: \(error.localizedDescription, privacy: .private)")
        }
    }

    /// Joins the given feeds into one session unless the break limit already
    /// did as each was logged.
    private static func joinFeeds(_ store: BabyStore, _ feeds: [FeedEntry], babyID: UUID) {
        guard Set(feeds.map(\.effectiveFeedingSessionID)).count > 1 else { return }
        do {
            try store.combineFeeds(feeds.map(\.id), for: babyID)
        } catch {
            AppLog.store.error("history sessions UI fixture could not join: \(error.localizedDescription, privacy: .private)")
        }
    }

    /// A Left part of 5:00 that ended six minutes ago, on an otherwise empty
    /// baby: inside the default 30-minute feed break, outside a 5-minute one.
    /// Lets the walkthrough prove the Settings limit decides the join without
    /// waiting six real minutes. Not part of `seedFixture`, whose own feeds
    /// could join this one and blur what History shows.
    static func seedFeedEndedSixMinutesAgo(_ store: BabyStore, babyID: UUID) {
        let now = store.now
        _ = try? store.logBreast(side: .left, from: now.addingTimeInterval(-660),
                                 to: now.addingTimeInterval(-360), for: babyID)
    }

    /// There is no backdated breast-feed verb — breast feeds are produced by the
    /// timer. This debug-only fixture inserts the already-completed historical
    /// record directly; using the timer would either wait in wall-clock time or
    /// manufacture the zero-duration feed that production now rejects.
    private static func backdatedBreastFeed(
        _ store: BabyStore,
        babyID: UUID,
        at start: Date,
        seconds: Int,
        side: BreastSide
    ) {
        let entry = FeedEntry(
            babyID: babyID,
            kind: .breast,
            startedAt: start,
            timeZoneID: TimeZone.current.identifier
        )
        entry.endedAt = start.addingTimeInterval(TimeInterval(seconds))
        entry.components = [.breast(id: UUID(), side: side, seconds: seconds)]
        store.context.insert(entry)
        try? store.context.save()
    }
}
#endif
