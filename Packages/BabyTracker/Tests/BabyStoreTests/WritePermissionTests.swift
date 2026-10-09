import Foundation
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Read-only caregiver write permission")
struct WritePermissionTests {
    @Test func viewerRealmRefusesEveryWritePathAndQueuesNothing() throws {
        let store = try BabyStore.sharedFixture()
        try store.setWritePermission(.viewer, for: Fixture.sharedRealm)

        #expect(throws: WritePermissionError.readOnlyCaregiver) {
            _ = try store.logDiaper(.wet, for: Fixture.babyID)
        }
        #expect(throws: WritePermissionError.readOnlyCaregiver) {
            _ = try store.startPart(.breast(.left), for: Fixture.babyID)
        }
        #expect(throws: WritePermissionError.readOnlyCaregiver) {
            _ = try store.logSleep(from: .now, to: nil, for: Fixture.babyID)
        }
        #expect(throws: WritePermissionError.readOnlyCaregiver) {
            _ = try store.addBaby(name: "Nope", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        }

        // Nothing was queued for the server either: a refused write must not
        // leave an intent behind that would be dispatched after a promotion.
        #expect(try store.pendingOutbox().isEmpty)
        #expect(try store.diaperCountForTesting() == 0)
    }

    @Test func caregiverAndOwnerBothWrite() throws {
        for role in [FamilyRole.caregiver, .owner] {
            let store = try BabyStore.sharedFixture()
            try store.setWritePermission(role, for: Fixture.sharedRealm)
            _ = try store.logDiaper(.wet, for: Fixture.babyID)
            #expect(try store.diaperCountForTesting() == 1)
        }
    }

    @Test func viewerCannotStopAnAlreadyRunningSleep() throws {
        let store = try BabyStore.sharedFixture()
        let timer = try store.startSleep(for: Fixture.babyID)
        try store.setWritePermission(.viewer, for: Fixture.sharedRealm)

        #expect(throws: WritePermissionError.readOnlyCaregiver) {
            _ = try store.stopSleep(timer)
        }
        #expect(try store.activeTimer(kind: .sleep, for: Fixture.babyID)?.id == timer.id)
    }

    /// The local realm is the user's own tracking and predates any family, so
    /// a family role never gates it.
    @Test func viewerInSharedRealmStillWritesFreelyInLocalRealm() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        try store.setWritePermission(.viewer, for: Fixture.sharedRealm)
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

        _ = try store.logDiaper(.wet, for: baby.id)
        #expect(try store.diaperCountForTesting() == 1)
        #expect(try store.writePermission(in: .local) == .owner)
    }

    /// Demotion must not strand work the caregiver legitimately recorded while
    /// they still had permission. The server will reject it, which is correct;
    /// silently dropping it here would lose their data.
    @Test func demotionKeepsAlreadyQueuedMutationsDispatchable() throws {
        let store = try BabyStore.sharedFixture()
        let entry = try store.logDiaper(.wet, for: Fixture.babyID)
        let queued = try #require(try store.pendingOutbox().first { $0.entityID == entry.id }).id

        try store.setWritePermission(.viewer, for: Fixture.sharedRealm)

        #expect(try store.nextDispatchBatch(limit: 10).map(\.id) == [queued])
    }

    /// The gate is durable, so a widget or App Intent process that never saw
    /// the demotion still refuses.
    @Test func writePermissionSurvivesReopen() throws {
        let harness = try LeaseTestHarness()
        try harness.store.activateRealmForTesting(Fixture.sharedRealm)
        try harness.store.setWritePermission(.viewer, for: Fixture.sharedRealm)

        let reopened = try harness.reopenStore()
        #expect(try reopened.writePermission(in: Fixture.sharedRealm) == .viewer)
        #expect(!reopened.canWriteInActiveRealm)
    }

    /// A shared realm nobody has recorded a role for behaves as it did before
    /// the role existed, rather than locking the user out.
    @Test func unknownSharedRealmDefaultsToCaregiver() throws {
        let store = try BabyStore.sharedFixture()
        #expect(try store.writePermission(in: Fixture.sharedRealm) == .caregiver)
        #expect(store.canWriteInActiveRealm)
    }

    @Test func promotionRestoresWriting() throws {
        let store = try BabyStore.sharedFixture()
        try store.setWritePermission(.viewer, for: Fixture.sharedRealm)
        #expect(throws: WritePermissionError.readOnlyCaregiver) {
            _ = try store.logDiaper(.wet, for: Fixture.babyID)
        }

        try store.setWritePermission(.caregiver, for: Fixture.sharedRealm)
        _ = try store.logDiaper(.wet, for: Fixture.babyID)
        #expect(try store.diaperCountForTesting() == 1)
    }

    @Test(arguments: [FamilyRole.owner, .caregiver, .viewer])
    func roleCapabilitiesAreTheSingleAuthority(role: FamilyRole) {
        #expect(role.canWrite == (role != .viewer))
        #expect(role.canManageMembers == (role == .owner))
        #expect(role.canInvite == (role == .owner))
        #expect(!role.displayName.isEmpty)
    }
}
