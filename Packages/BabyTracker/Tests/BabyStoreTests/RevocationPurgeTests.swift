import Foundation
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Revocation purge")
struct RevocationPurgeTests {
    private func storeWithBothRealms() throws -> (BabyStore, UUID, UUID) {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let localBaby = try store.addBaby(name: "Local", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        _ = try store.logDiaper(.wet, for: localBaby.id)

        let sharedBaby = Baby(id: UUID(), name: "Shared", birthDate: .now, realm: Fixture.sharedRealm)
        store.context.insert(sharedBaby)
        try store.context.save()
        try store.pointAtRealmForTesting(Fixture.sharedRealm)
        _ = try store.logDiaper(.dirty, for: sharedBaby.id)
        return (store, localBaby.id, sharedBaby.id)
    }

    /// The test the plan singles out. A committed authorization denial must
    /// leave nothing of the shared family on this device, even if the live
    /// close notification never arrived — and must leave the user's own local
    /// tracking untouched.
    @Test func stableRevocationPurgesSharedRealmAndKeepsLocal() throws {
        let (store, localBabyID, _) = try storeWithBothRealms()
        #expect(try store.babies(in: Fixture.sharedRealm).count == 1)

        try store.purgeSharedRealm(Fixture.sharedRealm)

        #expect(try store.babies(in: Fixture.sharedRealm).isEmpty)
        #expect(try store.babies(in: .local).map(\.id) == [localBabyID])
        #expect(store.activeRealm == .local)
    }

    /// Everything the family put on this device goes, not just the records:
    /// a surviving outbox row would try to sync to a family this device is no
    /// longer in, and a surviving cursor would make a later rejoin resume from
    /// a sequence that means nothing.
    @Test func purgeRemovesOutboxBasesAndCursorForThatRealm() throws {
        let (store, _, sharedBabyID) = try storeWithBothRealms()
        try store.applySyncPage(SyncFixture.page(highWater: SyncFixture.cursor(sequence: 5)))
        #expect(try store.syncCursor(in: Fixture.sharedRealm) != nil)
        #expect(!(try store.liveOutboxRows(in: Fixture.sharedRealm).isEmpty))
        _ = sharedBabyID

        try store.purgeSharedRealm(Fixture.sharedRealm)

        #expect(try store.liveOutboxRows(in: Fixture.sharedRealm).isEmpty)
        #expect(try store.syncCursor(in: Fixture.sharedRealm) == nil)
    }

    /// D10 is explicit: the shared rows are not recreated locally. A user who
    /// disconnects keeps what they tracked before joining and nothing the
    /// family shared afterwards.
    @Test func purgeNeverRecreatesSharedRowsLocally() throws {
        let (store, localBabyID, _) = try storeWithBothRealms()
        let localCountBefore = try store.babies(in: .local).count

        try store.purgeSharedRealm(Fixture.sharedRealm)

        #expect(try store.babies(in: .local).count == localCountBefore)
        #expect(try store.babies(in: .local).map(\.id) == [localBabyID])
        #expect(try store.diaperCountForTesting() == 1)
    }

    /// The local realm is permanent and is never a purge target, whatever it
    /// is asked to do.
    @Test func purgingTheLocalRealmIsRefused() throws {
        let (store, localBabyID, _) = try storeWithBothRealms()

        try store.purgeSharedRealm(.local)

        #expect(try store.babies(in: .local).map(\.id) == [localBabyID])
    }

    /// Another family's realm is untouched by one family's revocation.
    @Test func purgeIsScopedToTheNamedRealm() throws {
        let (store, _, _) = try storeWithBothRealms()
        let otherRealm = RealmID.shared(
            canonicalOrigin: "https://sync.example",
            serverInstanceID: UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!,
            familyID: UUID(uuidString: "99999999-9999-4999-8999-999999999999")!
        )
        let otherBaby = Baby(id: UUID(), name: "Other", birthDate: .now, realm: otherRealm)
        store.context.insert(otherBaby)
        try store.context.save()

        try store.purgeSharedRealm(Fixture.sharedRealm)

        #expect(try store.babies(in: otherRealm).map(\.id) == [otherBaby.id])
    }

    /// Purging twice is not an error: a retried revocation after a lost
    /// notification must be safe.
    @Test func purgeIsIdempotent() throws {
        let (store, localBabyID, _) = try storeWithBothRealms()

        try store.purgeSharedRealm(Fixture.sharedRealm)
        try store.purgeSharedRealm(Fixture.sharedRealm)

        #expect(try store.babies(in: .local).map(\.id) == [localBabyID])
        #expect(try store.babies(in: Fixture.sharedRealm).isEmpty)
    }
}
