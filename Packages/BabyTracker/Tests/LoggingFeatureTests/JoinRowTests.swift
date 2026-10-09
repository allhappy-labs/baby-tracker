import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

private func clockText(_ date: Date, zone: String = TimeZone.current.identifier) -> String {
    var style = Date.FormatStyle(date: .omitted, time: .shortened)
    style.timeZone = TimeZone(identifier: zone) ?? .gmt
    return date.formatted(style)
}

@Suite @MainActor struct JoinRowTests {
    @Test func labelAndGapNameThePreviousFeed() throws {
        let (store, baby, clock) = try fixture()
        let earlier = clock.now.addingTimeInterval(-5 * 3600)
        let previous = try store.logBreast(side: .left, from: earlier, to: earlier.addingTimeInterval(45 * 60), for: baby)
        let later = earlier.addingTimeInterval(45 * 60 + 2 * 3600 + 8 * 60)
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: later)

        let model = JoinRowModel(store: store, babyID: baby, kind: .feed, rootID: bottle.id)

        #expect(model.previous?.rootID == previous.id)
        #expect(model.label == "Join the \(clockText(earlier, zone: previous.timeZoneID)) feed")
        #expect(model.gapText == "2h 8m before")
    }

    /// Review Focus 3: solids are never a join target.
    @Test func absentWhenTheOnlyEarlierFeedIsSolids() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logSolids(foods: ["Pear"], amount: .some, for: baby, at: clock.now.addingTimeInterval(-3 * 3600))
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-600))

        let model = JoinRowModel(store: store, babyID: baby, kind: .feed, rootID: bottle.id)

        #expect(model.previous == nil)
    }

    @Test func absentWhenNothingCameBefore() throws {
        let (store, baby, clock) = try fixture()
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-600))

        #expect(JoinRowModel(store: store, babyID: baby, kind: .feed, rootID: bottle.id).previous == nil)
    }

    @Test func joinPutsBothInOneSession() throws {
        let (store, baby, clock) = try fixture()
        let earlier = clock.now.addingTimeInterval(-5 * 3600)
        let previous = try store.logBreast(side: .left, from: earlier, to: earlier.addingTimeInterval(600), for: baby)
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-600))
        let model = JoinRowModel(store: store, babyID: baby, kind: .feed, rootID: bottle.id)

        try model.join()

        #expect(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).count == 1)
        #expect(bottle.effectiveFeedingSessionID == previous.id)
        #expect(model.previous == nil)
    }

    /// Final review I3, for the lone part editor's row: once joined, the
    /// row offers nothing more until its editor opens again.
    @Test func aJoinedRowOffersNoFurtherJoin() throws {
        let (store, baby, clock) = try fixture()
        let at = { (hours: Double) in clock.now.addingTimeInterval(-hours * 3_600) }
        let first = try store.logBreast(side: .left, from: at(9), to: at(9).addingTimeInterval(600), for: baby)
        let second = try store.logBreast(side: .left, from: at(6), to: at(6).addingTimeInterval(600), for: baby)
        let third = try store.logBreast(side: .left, from: at(3), to: at(3).addingTimeInterval(600), for: baby)
        let model = JoinRowModel(store: store, babyID: baby, kind: .feed, partOf: third.id)

        try model.join()
        #expect(model.previous == nil)
        try model.join()
        model.reload()

        #expect(model.previous == nil)
        #expect(first.effectiveFeedingSessionID == first.id)
        #expect(third.effectiveFeedingSessionID == second.id)
        #expect(JoinRowModel(store: store, babyID: baby, kind: .feed, partOf: third.id).previous?.rootID == first.id)
    }

    /// The row names its session by any member, as the session calls do.
    @Test func joinWorksFromALaterMemberOfTheSession() throws {
        let (store, baby, clock) = try fixture()
        let earlier = clock.now.addingTimeInterval(-6 * 3600)
        let first = try store.logBreast(side: .left, from: earlier, to: earlier.addingTimeInterval(600), for: baby)
        let base = clock.now.addingTimeInterval(-3600)
        let left = try store.logBreast(side: .left, from: base, to: base.addingTimeInterval(600), for: baby)
        let right = try store.addFeedPart(.breast(side: .right, from: base.addingTimeInterval(700), to: base.addingTimeInterval(1300)),
                                          toSession: left.id, for: baby)
        let model = JoinRowModel(store: store, babyID: baby, kind: .feed, rootID: right.id)

        #expect(model.gapText == "4h 50m before")
        try model.join()

        #expect(left.effectiveFeedingSessionID == first.id)
        #expect(right.effectiveFeedingSessionID == first.id)
    }

    @Test func sleepRowNamesThePreviousSleep() throws {
        let (store, baby, clock) = try fixture()
        let earlier = clock.now.addingTimeInterval(-6 * 3600)
        let previous = try store.logSleep(from: earlier, to: earlier.addingTimeInterval(3600), for: baby)
        let later = try store.logSleep(from: earlier.addingTimeInterval(3 * 3600), to: earlier.addingTimeInterval(4 * 3600), for: baby)

        let model = JoinRowModel(store: store, babyID: baby, kind: .sleep, rootID: later.id)

        #expect(model.label == "Join the \(clockText(earlier, zone: previous.timeZoneID)) sleep")
        #expect(model.gapText == "2h before")
        try model.join()
        #expect(later.effectiveSleepSessionID == previous.id)
    }

    @Test func reloadNoticesAnEarlierSessionLoggedLater() throws {
        let (store, baby, clock) = try fixture()
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-600))
        let model = JoinRowModel(store: store, babyID: baby, kind: .feed, rootID: bottle.id)
        #expect(model.previous == nil)

        _ = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-4 * 3600))
        model.reload()

        #expect(model.previous != nil)
    }

    // MARK: - A session id that is also a member's id (final review I2)

    /// The session sheet holds a session id; the lone part editor holds an
    /// entry id. When one id is both, each takes its own meaning.
    @Test func aSessionIdAndAnEntryIdEachNameTheirOwnSession() throws {
        let (store, baby, clock) = try fixture()
        let (z, a, b, x, _) = try collidingOrphan(store, baby, clock)

        let sessionRow = JoinRowModel(store: store, babyID: baby, kind: .feed, rootID: b.id)
        let partRow = JoinRowModel(store: store, babyID: baby, kind: .feed, partOf: b.id)

        #expect(sessionRow.previous?.rootID == a.id)
        #expect(partRow.previous?.rootID == z.id)
        try sessionRow.join()
        #expect(x.effectiveFeedingSessionID == a.id)
        #expect(z.effectiveFeedingSessionID == z.id)
    }
}
