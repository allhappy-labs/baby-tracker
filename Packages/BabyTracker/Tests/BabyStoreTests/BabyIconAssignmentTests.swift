import Testing
import Foundation
import BabyCore
import BabyProtocol
@testable import BabyStore

@MainActor
struct BabyIconAssignmentTests {
    @Test func addBabyAssignsFromTheSet() throws {
        let store = try makeStore()
        let first = try store.addBaby(name: "Max", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        let twin = try store.addBaby(name: "Leo", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        let sister = try store.addBaby(name: "Sophia", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        #expect(first.iconID == "boy.1")
        #expect(twin.iconID == "boy.2")
        #expect(sister.iconID == "girl.1")
        // Set before the create was recorded, so the create's payload carries it.
        #expect(sister.payloadFields()["icon"] as? String == "girl.1")
    }

    @Test func aDeletedSiblingFreesItsIcon() throws {
        let store = try makeStore()
        let a = try store.addBaby(name: "A", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        _ = try store.addBaby(name: "B", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        try store.deleteBaby(a)
        let c = try store.addBaby(name: "C", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        #expect(c.iconID == "girl.1")
    }

    @Test func changingSexReassignsFromTheOtherSet() throws {
        let store = try makeStore()
        let baby = try store.addBaby(name: "Kim", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        try store.updateBabyAndGrowthProfile(baby, name: "Kim", birthDate: baby.birthDate, referenceSex: .girls, expectedDueDate: nil)
        #expect(baby.iconID == "girl.1")
    }

    @Test func savingWithTheSameSexKeepsTheIcon() throws {
        let store = try makeStore()
        _ = try store.addBaby(name: "A", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        let b = try store.addBaby(name: "B", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        try store.updateBabyAndGrowthProfile(b, name: "Bee", birthDate: b.birthDate, referenceSex: .boys, expectedDueDate: nil)
        #expect(b.iconID == "boy.2")
    }

    @Test func settingSexForTheFirstTimeAssigns() throws {
        let store = try makeStore()
        let legacy = try insertLegacyBaby(in: store, name: "Old", sortOrder: 0)   // no profile, no icon
        try store.updateBabyAndGrowthProfile(legacy, name: "Old", birthDate: legacy.birthDate, referenceSex: .girls, expectedDueDate: nil)
        #expect(legacy.iconID == "girl.1")
    }

    @Test func fillInSkipsBabiesWithoutSex() throws {
        let store = try makeStore()
        let legacy = try insertLegacyBaby(in: store, name: "Old", sortOrder: 0)
        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: false) == 0)
        #expect(legacy.iconID == nil)
    }

    @Test func fillInAssignsInOrderAndIsIdempotent() throws {
        let store = try makeStore()
        let second = try insertLegacyBaby(in: store, name: "Second", sortOrder: 1)
        let first = try insertLegacyBaby(in: store, name: "First", sortOrder: 0)
        let keeper = try insertLegacyBaby(in: store, name: "Keeper", sortOrder: 2)
        keeper.iconID = "boy.1"
        try store.setGrowthProfile(for: first.id, referenceSex: .boys, expectedDueDate: nil)
        try store.setGrowthProfile(for: second.id, referenceSex: .boys, expectedDueDate: nil)
        try store.setGrowthProfile(for: keeper.id, referenceSex: .boys, expectedDueDate: nil)

        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: false) == 2)
        #expect(first.iconID == "boy.2")
        #expect(second.iconID == "boy.3")
        let revision = store.revision
        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: false) == 0)
        #expect(store.revision == revision)
    }

    @Test func fillInInAReadOnlyRealmWritesNothing() throws {
        // As GrowthStoreTests.viewerFailuresKeepEditAndDeletionStateUntouched:
        // build the shared realm while writable, then demote to viewer.
        let store = try makeStore()
        try store.setActiveRealm(Fixture.sharedRealm)
        let baby = try insertLegacyBaby(in: store, name: "Shared", sortOrder: 0)
        try store.setGrowthProfile(for: baby.id, referenceSex: .girls, expectedDueDate: nil)
        try store.setWritePermission(.viewer, for: Fixture.sharedRealm)
        let revision = store.revision
        let logs = try store.changeLog().count

        #expect(throws: (any Error).self) { try store.assignMissingBabyIcons(sharedRealmSynced: true) }
        #expect(baby.iconID == nil)
        #expect(store.revision == revision)
        #expect(try store.changeLog().count == logs)
    }

    // MARK: - Launch gate (final review F1)

    @Test func fillInAdoptsTheServerCopysIconWithoutRecording() throws {
        let store = try makeStore()
        try store.setActiveRealm(Fixture.sharedRealm)
        let baby = try insertLegacyBaby(in: store, name: "Mia", sortOrder: 0)
        try store.setGrowthProfile(for: baby.id, referenceSex: .girls, expectedDueDate: nil)
        // An older build stored the other phone's icon only in the base.
        try putBase(for: baby, icon: "girl.2", in: store)
        let logs = try store.changeLog().count
        let outbox = try store.pendingOutbox().count

        // Not synced yet: adopting is server state, so it runs anyway.
        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: false) == 1)
        #expect(baby.iconID == "girl.2")
        #expect(try store.changeLog().count == logs)
        #expect(try store.pendingOutbox().count == outbox)
        let revision = store.revision
        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: true) == 0)
        #expect(store.revision == revision)
        #expect(try store.changeLog().count == logs)
    }

    @Test func fillInInASharedRealmAssignsOnlyAfterASync() throws {
        let store = try makeStore()
        try store.setActiveRealm(Fixture.sharedRealm)
        let baby = try insertLegacyBaby(in: store, name: "Leo", sortOrder: 0)
        try store.setGrowthProfile(for: baby.id, referenceSex: .boys, expectedDueDate: nil)
        let logs = try store.changeLog().count
        let outbox = try store.pendingOutbox().count
        let revision = store.revision

        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: false) == 0)
        #expect(baby.iconID == nil)
        #expect(try store.changeLog().count == logs)
        #expect(try store.pendingOutbox().count == outbox)
        #expect(store.revision == revision)

        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: true) == 1)
        #expect(baby.iconID == "boy.1")
        #expect(try store.changeLog().count == logs + 1)
        #expect(try store.pendingOutbox().count == outbox + 1)
    }

    @Test func fillInInTheLocalRealmIgnoresTheSyncGate() throws {
        let store = try makeStore()
        let baby = try insertLegacyBaby(in: store, name: "Solo", sortOrder: 0)
        try store.setGrowthProfile(for: baby.id, referenceSex: .girls, expectedDueDate: nil)
        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: false) == 1)
        #expect(baby.iconID == "girl.1")
    }

    @Test func fillInReassignsAnIconFromTheWrongSet() throws {
        let store = try makeStore()
        let baby = try insertLegacyBaby(in: store, name: "Ava", sortOrder: 0)
        try store.setGrowthProfile(for: baby.id, referenceSex: .girls, expectedDueDate: nil)
        baby.iconID = "boy.1"
        let logs = try store.changeLog().count

        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: false) == 1)
        #expect(baby.iconID == "girl.1")
        #expect(try store.changeLog().count == logs + 1)
        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: false) == 0)
    }

    @Test func fillInDoesNotAdoptABaseIconFromTheWrongSet() throws {
        let store = try makeStore()
        try store.setActiveRealm(Fixture.sharedRealm)
        let baby = try insertLegacyBaby(in: store, name: "Ava", sortOrder: 0)
        try store.setGrowthProfile(for: baby.id, referenceSex: .girls, expectedDueDate: nil)
        try putBase(for: baby, icon: "boy.3", in: store)
        let logs = try store.changeLog().count

        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: false) == 0)
        #expect(baby.iconID == nil)
        #expect(try store.changeLog().count == logs)
        #expect(try store.assignMissingBabyIcons(sharedRealmSynced: true) == 1)
        #expect(baby.iconID == "girl.1")
        #expect(try store.changeLog().count == logs + 1)
    }

    @Test func savingWithTheSameSexReplacesAWrongSetIcon() throws {
        let store = try makeStore()
        let baby = try store.addBaby(name: "Ava", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        baby.iconID = "boy.2"
        try store.updateBabyAndGrowthProfile(baby, name: "Ava", birthDate: baby.birthDate, referenceSex: .girls, expectedDueDate: nil)
        #expect(baby.iconID == "girl.1")
    }

    // MARK: - Helpers

    /// The server-canonical copy this phone last received for `baby`.
    private func putBase(for baby: Baby, icon: String, in store: BabyStore) throws {
        var fields = baby.payloadFields()
        fields["icon"] = icon
        try store.replaceBase(entityID: baby.id, kind: .baby, version: 3,
                              payload: JSONSerialization.data(withJSONObject: fields))
        try store.context.save()
    }

    private func makeStore() throws -> BabyStore {
        try BabyStore.inMemory(dateProvider: TestDateProvider(Date(timeIntervalSince1970: 1_790_000_000)))
    }

    /// A baby from before profiles existed: no growth profile, no icon.
    private func insertLegacyBaby(in store: BabyStore, name: String, sortOrder: Int) throws -> Baby {
        let baby = Baby(name: name, birthDate: Date(timeIntervalSince1970: 1_780_000_000),
                        sortOrder: sortOrder, realm: store.activeRealm)
        store.context.insert(baby)
        try store.record(.create, on: baby)
        return baby
    }
}
