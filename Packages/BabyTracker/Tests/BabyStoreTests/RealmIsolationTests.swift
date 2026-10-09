import Foundation
import Testing
@testable import BabyCore
@testable import BabyStore

@MainActor
@Suite("Realm isolation")
struct RealmIsolationTests {
    @Test func sameUUIDIsolatedAcrossLocalAndSharedRealms() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let id = UUID(uuidString: "10000000-0000-4000-8000-000000000001")!
        let local = Baby(id: id, name: "Local", birthDate: .now, realm: .local)
        let shared = Baby(id: id, name: "Shared", birthDate: .now, realm: .testShared)
        store.context.insert(local)
        store.context.insert(shared)
        try store.context.save()

        #expect(try store.babies(in: .local).map(\.name) == ["Local"])
        #expect(try store.babies(in: .testShared).map(\.name) == ["Shared"])
    }

    @Test func localOnlyWriteNeedsNoAccountAndCreatesNoSyncOutbox() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))

        _ = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

        #expect(try store.babies(in: .local).count == 1)
        #expect(try store.pendingOutbox(in: .local).isEmpty)
    }
}

private extension RealmID {
    static let testShared = RealmID.shared(
        canonicalOrigin: "https://sync.example",
        serverInstanceID: UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!,
        familyID: UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
    )
}

@MainActor
@Suite("Every write is born in the active realm")
struct ActiveRealmWriteTests {
    /// Regression. Every record-creating API built its entity with the model's
    /// default realm — `.local` — so a feed, sleep, diaper or bottle logged
    /// while a shared family was active was written into the user's PRIVATE
    /// realm. It would never sync, it would never appear for other caregivers,
    /// and disconnecting would not remove it because the purge is realm-scoped.
    ///
    /// Found by the revocation purge test: a shared diaper survived a purge
    /// that had correctly removed the shared baby.
    @Test func everyRecordWriteLandsInTheActiveSharedRealm() throws {
        let store = try BabyStore.sharedFixture()

        let diaper = try store.logDiaper(.wet, for: Fixture.babyID)
        let sleep = try store.logSleep(from: .now, to: .now, for: Fixture.babyID)
        let bottle = try store.logBottle(volumeML: 90, contents: .formula, for: Fixture.babyID)
        let solids = try store.logSolids(foods: ["pear"], amount: .some, for: Fixture.babyID)

        #expect(diaper.realmKey == Fixture.sharedRealm.encodedKey)
        #expect(sleep.realmKey == Fixture.sharedRealm.encodedKey)
        #expect(bottle.realmKey == Fixture.sharedRealm.encodedKey)
        #expect(solids.realmKey == Fixture.sharedRealm.encodedKey)

        // And each one is queued for the family, which only holds if the row
        // is in the realm the outbox is scoped to.
        #expect(try store.liveOutboxRows(in: Fixture.sharedRealm).count == 4)
    }

    /// A completed timer's record, too — those go through a different path.
    @Test func timerCompletionsLandInTheActiveSharedRealm() throws {
        let clock = TestDateProvider(.now)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let timer = try store.startPart(.breast(.left), for: Fixture.babyID)
        clock.advance(by: 30)
        let feed = try #require(try store.stopFeed(timer))

        let sleepTimer = try store.startSleep(for: Fixture.babyID)
        let sleep = try store.stopSleep(sleepTimer)

        #expect(feed.realmKey == Fixture.sharedRealm.encodedKey)
        #expect(sleep.realmKey == Fixture.sharedRealm.encodedKey)
    }

    /// Local-only use is unaffected: with no family, everything is local.
    @Test func localOnlyWritesStayLocal() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

        let diaper = try store.logDiaper(.wet, for: baby.id)

        #expect(diaper.realmKey == RealmID.local.encodedKey)
        #expect(try store.pendingOutbox().isEmpty)
    }
}
