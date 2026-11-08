import Testing
import Foundation
import BabyCore
@testable import BabyStore

/// Home rebuilds its snapshot only when `store.revision` moves (ruling R13),
/// so a write that forgets to bump it leaves Home stale until something else
/// happens to. Every write a screen can trigger is listed here.
@MainActor @Suite struct StoreRevisionTests {
    @Test func everyWriteBumpsTheStoreRevision() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.inMemory(dateProvider: clock)

        /// Runs `write` and fails naming it if the revision did not move.
        func bumps(_ name: String, _ write: () throws -> Void) throws {
            let before = store.revision
            try write()
            #expect(store.revision > before, "\(name) did not bump the store revision")
        }

        var babyID = UUID()
        try bumps("addBaby") { babyID = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil).id }
        try bumps("logDiaper") { _ = try store.logDiaper(.wet, for: babyID) }
        try bumps("logBottle") {
            _ = try store.logBottle(volumeML: 90, contents: .formula, for: babyID)
        }
        try bumps("logSleep") {
            _ = try store.logSleep(from: clock.now.addingTimeInterval(-60), to: clock.now, for: babyID)
        }

        var feedTimer: ActiveTimer?
        try bumps("startFeed") { feedTimer = try store.startPart(.breast(.left), for: babyID) }
        let feed = try #require(feedTimer)
        clock.advance(by: 1)
        try bumps("detachFromSession") { try store.detachFromSession(feed) }
        try bumps("setLiveActivityID") { try store.setLiveActivityID("activity-1", on: feed) }
        clock.advance(by: 1)
        try bumps("stopFeed") { _ = try store.stopFeed(feed) }

        var sleepTimer: ActiveTimer?
        try bumps("startSleep") { sleepTimer = try store.startSleep(for: babyID) }
        let sleep = try #require(sleepTimer)
        try bumps("stopSleep") { _ = try store.stopSleep(sleep) }

        let diaper = try #require(try store.lastDiaper(for: babyID))
        try bumps("update") { try store.update(diaper) }
        try bumps("softDelete") { try store.softDelete(diaper) }
    }

    @Test func everyOnePartAndSessionWriteBumpsTheStoreRevision() throws {
        let (store, babyID, clock) = try fixture()

        func bumps(_ name: String, _ write: () throws -> Void) throws {
            let before = store.revision
            try write()
            #expect(store.revision > before, "\(name) did not bump the store revision")
        }

        var left: ActiveTimer?
        try bumps("startPart breast") { left = try store.startPart(.breast(.left), for: babyID) }
        clock.advance(by: 300)
        _ = try store.stopFeed(try #require(left))

        // Joins the left part's session, so there is a link to detach.
        var right: ActiveTimer?
        try bumps("startPart joined") { right = try store.startPart(.breast(.right), for: babyID) }
        let joined = try #require(right)
        #expect(joined.sessionID != nil)
        try bumps("detachFromSession") { try store.detachFromSession(joined) }
        clock.advance(by: 300)
        _ = try store.stopFeed(joined)

        var bottle: ActiveTimer?
        try bumps("startPart bottle") { bottle = try store.startPart(.bottle(.formula), for: babyID) }
        clock.advance(by: 300)
        var bottleFeed: FeedEntry?
        try bumps("stopBottle") { bottleFeed = try store.stopBottle(try #require(bottle), volumeML: 80) }
        let saved = try #require(bottleFeed)
        try bumps("setBottle") { try store.setBottle(volumeML: 90, contents: .formula, onFeed: saved.id) }

        clock.advance(by: 3_600)
        try bumps("logBreast") {
            _ = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-600),
                                    to: clock.now.addingTimeInterval(-60), for: babyID)
        }

        // Two sleeps further apart than the sleep break limit: two sessions.
        let first = try store.logSleep(from: clock.now.addingTimeInterval(-7_200),
                                       to: clock.now.addingTimeInterval(-5_400), for: babyID)
        let second = try store.logSleep(from: clock.now.addingTimeInterval(-3_600),
                                        to: clock.now.addingTimeInterval(-1_800), for: babyID)
        try bumps("combineSleeps") { try store.combineSleeps([first.id, second.id], for: babyID) }
        try bumps("separateSleep") { try store.separateSleep(at: second.id, for: babyID) }
    }
}
