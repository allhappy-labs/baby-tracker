import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor @Suite struct OnePartTimerTests {
    @Test func leftStopThenRightWithinTheLimitIsOneFeed() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.startPart(.breast(.left), for: baby)
        clock.advance(by: 540)
        let leftEntry = try #require(try store.stopFeed(left))
        clock.advance(by: 600)
        let right = try store.startPart(.breast(.right), for: baby)
        #expect(right.sessionID == leftEntry.effectiveFeedingSessionID)
        clock.advance(by: 260)
        let rightEntry = try #require(try store.stopFeed(right))
        #expect(rightEntry.feedingSessionID == leftEntry.id)
        #expect(rightEntry.components.count == 1)
        let sessions = try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.count == 1)
        #expect(sessions[0].activeSeconds == 540 + 260)
    }

    @Test func pastTheLimitIsTwoFeeds() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        clock.advance(by: 1_801)
        let right = try store.startPart(.breast(.right), for: baby)
        #expect(right.sessionID == nil)
    }

    @Test func tappingTheOtherSideWhileOneRunsStopsItAndJoins() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.startPart(.breast(.left), for: baby)
        clock.advance(by: 120)
        let right = try store.startPart(.breast(.right), for: baby)
        #expect(right.id != left.id)
        #expect(try store.activeTimers(for: baby).count == 1)
        let lastFeed = try #require(try store.lastFeed(for: baby))
        #expect(lastFeed.components.first?.breastSide == .left)
        #expect(right.sessionID == lastFeed.effectiveFeedingSessionID)
    }

    @Test func tappingTheRunningSideAgainIsIgnored() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.startPart(.breast(.left), for: baby)
        clock.advance(by: 5)
        #expect(try store.startPart(.breast(.left), for: baby).id == left.id)
    }

    @Test func aDetachedPartStartsItsOwnFeed() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        let right = try store.startPart(.breast(.right), for: baby)
        try store.detachFromSession(right)
        clock.advance(by: 300)
        let entry = try #require(try store.stopFeed(right))
        #expect(entry.feedingSessionID == nil)
        #expect(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).count == 2)
    }

    @Test func detachingDoesNotMoveTheTimersStart() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        let right = try store.startPart(.breast(.right), for: baby)
        let originalStart = right.startedAt
        try store.detachFromSession(right)
        #expect(right.startedAt == originalStart)
        #expect(right.sessionID == nil)
    }

    @Test func aTimedBottleStopsWithItsAmountAndJoins() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        let bottle = try store.startPart(.bottle(.breastMilk), for: baby)
        clock.advance(by: 400)
        let entry = try #require(try store.stopBottle(bottle, volumeML: 90))
        #expect(entry.totalBottleVolumeML == 90)
        #expect(entry.bottleComponents.first?.bottleContents == .breastMilk)
        #expect(entry.feedingSessionID != nil)
    }

    @Test func aChipLoggedBottleAndAPastBreastJoin() throws {
        let (store, baby, clock) = try fixture()
        let first = try #require(try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300)))
        clock.advance(by: 120)
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        #expect(bottle.feedingSessionID == first.id)
        let past = try store.logBreast(side: .right, from: clock.now.addingTimeInterval(30), to: clock.now.addingTimeInterval(400), for: baby)
        #expect(past.feedingSessionID == first.id)
    }

    @Test func sleepWakeSleepWithinFifteenMinutesIsOneSleep() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 2_400))
        clock.advance(by: 360)
        let second = try store.startSleep(for: baby)
        #expect(second.sessionID == first.id)
        clock.advance(by: 1_900)
        let entry = try store.stopSleep(second)
        #expect(entry.sleepSessionID == first.id)
        #expect(try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture).count == 1)
    }

    @Test func aStartCannotBeMovedBeforeThePreviousPartEnded() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        clock.advance(by: 60)
        let right = try store.startPart(.breast(.right), for: baby)
        #expect(throws: PartError.startBeforePreviousPart) {
            try store.editRunningTimer(id: right.id, startedAt: clock.now.addingTimeInterval(-120), components: [])
        }
    }

    @Test func aShortAccidentalTapLeavesNoPartButKeepsTheSession() throws {
        let (store, baby, clock) = try fixture()
        let first = try #require(try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300)))
        let tap = try store.startPart(.breast(.right), for: baby)
        clock.advance(by: 10)
        #expect(try store.stopFeed(tap) == nil)
        clock.advance(by: 30)
        #expect(try store.startPart(.breast(.right), for: baby).sessionID == first.id)
    }

    @Test func aDiaperNoteIsSavedInTheSameWrite() throws {
        let (store, baby, _) = try fixture()
        let diaper = try store.logDiaper(.mixed, for: baby, note: "after bath")
        #expect(diaper.note == "after bath")
    }

    // Solids never join and are never joined (global-constraints.md).
    @Test func aSolidsLogRightAfterAFeedPartNeverJoins() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        clock.advance(by: 10)
        let solids = try store.logSolids(foods: ["pear"], amount: .some, for: baby)
        #expect(solids.feedingSessionID == nil)
    }

    // A timer started with a joined session must send `session_id` as the
    // lowercased UUID string in its start payload, not merely a non-null key
    // (Task 6 only verified the key is present and null).
    @Test func aPartStartedWithAJoinedSessionSendsItAsAStringInItsStartPayload() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        clock.advance(by: 120)
        let right = try store.startPart(.breast(.right), for: baby)
        let sessionID = try #require(right.sessionID)
        #expect(MutationFactory.timerPayload(right)["session_id"] as? String == sessionID.uuidString.lowercased())
    }

    // A stopped joined sleep must carry `sleep_session_id` as the lowercased
    // UUID string in its payload fields, not merely a non-null key.
    @Test func aStoppedJoinedSleepSendsItsSessionLinkAsAStringInItsPayload() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 2_400))
        clock.advance(by: 360)
        let entry = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 1_900))
        #expect(entry.sleepSessionID == first.id)
        let sessionID = try #require(entry.sleepSessionID)
        #expect(entry.payloadFields()["sleep_session_id"] as? String == sessionID.uuidString.lowercased())
    }

    // MARK: Fix round 1 — review findings

    // (1a) `startPart` → `stopBottle` → `finishBottle` used to throw
    // `BottleTimerError.emptyDuration` for a bottle abandoned inside the same
    // second, leaving it running. It must now be discarded silently, like an
    // accidental breast tap under 30 s.
    @Test func switchingAwayFromABottleWithinASecondDiscardsItWithoutThrowing() throws {
        let (store, baby, _) = try fixture()
        let bottle = try store.startPart(.bottle(.formula), for: baby)
        let left = try store.startPart(.breast(.left), for: baby)
        #expect(left.id != bottle.id)
        #expect(try store.activeTimers(for: baby).count == 1)
        #expect(try store.lastFeed(for: baby) == nil)
    }

    // (1b) Past a second, the usual stop-and-join path runs: the bottle is
    // saved with the habitual volume and the new breast timer joins its session.
    @Test func switchingAwayFromABottleAfterItRanStopsItWithTheUsualAmountAndJoins() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.startPart(.bottle(.formula), for: baby)
        clock.advance(by: 120)
        let left = try store.startPart(.breast(.left), for: baby)
        let savedBottle = try #require(try store.lastFeed(for: baby))
        let usualVolume = try LogDefaults(store: store).mostUsedBottleVolumeML(for: baby)
        #expect(savedBottle.totalBottleVolumeML == usualVolume)
        #expect(left.sessionID == savedBottle.effectiveFeedingSessionID)
    }

    // (3 / R12) A bottle this device didn't start (a remote timer applied
    // from another phone) carries no contents. `stopBottle` must fall back
    // to what was logged last, not the generic default — distinguishable
    // here because the last logged bottle is breast milk, not the fallback
    // (formula). Built directly, as remote apply (`RemoteTimerState`) leaves
    // it: a running bottle with no `bottleContents` set.
    @Test func aBottleStartedElsewhereFallsBackToTheLastLoggedContentsNotTheGenericDefault() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logBottle(volumeML: 80, contents: .breastMilk, for: baby)
        clock.advance(by: 300)
        let timer = ActiveTimer(babyID: baby, kind: .feed, startedAt: clock.now, timeZoneID: "UTC")
        timer.currentBottleStartedAt = clock.now
        store.context.insert(timer)
        try store.context.save()
        #expect(timer.bottleContents == nil)
        clock.advance(by: 120)
        let entry = try #require(try store.stopBottle(timer, volumeML: 90))
        #expect(entry.bottleComponents.first?.bottleContents == .breastMilk)
    }

    // (4) `setBottleVolume` must refuse a feed that doesn't have exactly one
    // bottle component, and a volume that isn't positive.
    @Test func setBottleVolumeRewritesTheBottleComponent() throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        try store.setBottleVolume(90, onFeed: feed.id)
        #expect(feed.bottleComponents.map(\.volumeML) == [90])
    }

    @Test func setBottleVolumeRefusesAFeedWithNoBottleComponent() throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logSolids(foods: ["pear"], amount: .some, for: baby)
        #expect(throws: PartError.notASingleBottle) {
            try store.setBottleVolume(80, onFeed: feed.id)
        }
    }

    @Test func setBottleVolumeRefusesAFeedWithTwoBottleComponents() throws {
        let (store, baby, _) = try fixture()
        // Two bottle components on one entry can no longer be built by a
        // live timer — one-part timers give each bottle its own timer, and
        // `combineFeeds` links entries rather than merging their components.
        // Built directly, as `setBottleVolume` only cares what is saved.
        let feed = FeedEntry(babyID: baby, kind: .bottle, startedAt: store.now, timeZoneID: "UTC")
        feed.components = [
            .bottle(id: UUID(), volumeML: 40, contents: .formula),
            .bottle(id: UUID(), volumeML: 30, contents: .formula),
        ]
        feed.endedAt = store.now
        store.context.insert(feed)
        try store.record(.create, on: feed)
        #expect(throws: PartError.notASingleBottle) {
            try store.setBottleVolume(80, onFeed: feed.id)
        }
    }

    @Test func setBottleVolumeRefusesANonPositiveVolume() throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        #expect(throws: PartError.invalidVolume) {
            try store.setBottleVolume(0, onFeed: feed.id)
        }
    }

    // (5) `logBreast` throws `PartError.tooShort` under 30 s.
    @Test func logBreastThrowsUnderThirtySeconds() throws {
        let (store, baby, clock) = try fixture()
        #expect(throws: PartError.tooShort) {
            try store.logBreast(side: .left, from: clock.now, to: clock.now.addingTimeInterval(29), for: baby)
        }
    }

    // (5) The `editRunningTimer` guard throws only when the new start is
    // strictly before the previous part's end — exactly at the boundary is allowed.
    @Test func aStartExactlyAtThePreviousPartsEndIsAllowed() throws {
        let (store, baby, clock) = try fixture()
        let leftEntry = try #require(try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300)))
        clock.advance(by: 60)
        let right = try store.startPart(.breast(.right), for: baby)
        let previousEnd = try #require(leftEntry.endedAt)
        try store.editRunningTimer(id: right.id, startedAt: previousEnd, components: [])
        #expect(right.startedAt == previousEnd)
    }
}
