import Foundation
import SwiftData
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

/// The pure ordering rule between what this phone can vouch for and what a
/// pushed card already shows, the duplicate winner, and the durable revision
/// reader those decisions are fed from.
@MainActor
@Suite("Live Activity reconciliation")
struct LiveActivityReconciliationTests {
    static let instance = UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!
    static let family = UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
    static let generation = UUID(uuidString: "a1000000-0000-4000-8000-000000000001")!
    static let otherGeneration = UUID(uuidString: "a2000000-0000-4000-8000-000000000002")!
    static let timer = UUID(uuidString: "a3000000-0000-4000-8000-000000000003")!

    static func scope(baby: UUID = Fixture.babyID, kind: String = "feed", epoch: Int64 = 2) -> LiveActivityScope {
        try! LiveActivityScope(serverInstanceID: instance, recoveryEpoch: epoch, familyID: family, babyID: baby, kindRaw: kind)
    }

    static func revision(generation: UUID = generation, timer: UUID? = timer, sequence: Int64, version: Int64 = 1,
                         timestamp: Int64 = 1_800_000_000) -> LiveActivityRevision {
        try! LiveActivityRevision(scope: scope(), generationID: generation, timerID: timer, familySequence: sequence,
                                  presentationVersion: version, presentationTimestamp: timestamp, canWrite: true)
    }

    // MARK: decide

    @Test func aCardFromAnotherRealmIsEnded() {
        let remote = Self.revision(sequence: 4)
        for pending in [false, true] {
            #expect(LiveActivityReconciliation.decide(local: Self.revision(sequence: 9), remote: remote,
                                                      hasPendingLocalMutation: pending, sameScope: false) == .end)
            #expect(LiveActivityReconciliation.decide(local: nil, remote: nil, hasPendingLocalMutation: pending, sameScope: false) == .end)
        }
    }

    /// A local or legacy card has never shown server content: nothing pushed
    /// can be overwritten, so it keeps today's immediate behaviour.
    @Test func aCardWithNoServerRevisionIsUpdatedImmediately() {
        #expect(LiveActivityReconciliation.decide(local: nil, remote: nil, hasPendingLocalMutation: true, sameScope: true) == .apply)
        #expect(LiveActivityReconciliation.decide(local: Self.revision(sequence: 2), remote: nil, hasPendingLocalMutation: false,
                                                  sameScope: true) == .apply)
    }

    /// Nothing acknowledged here to compare with — a fresh join, a cursor
    /// from another epoch — and the card has server content: leave it.
    @Test func withoutAnAcknowledgedCursorAPushedCardIsPreserved() {
        #expect(LiveActivityReconciliation.decide(local: nil, remote: Self.revision(sequence: 4), hasPendingLocalMutation: false,
                                                  sameScope: true) == .preserveUntilSync)
    }

    @Test func aCardNewerThanTheStoreIsPreservedUntilSync() {
        #expect(LiveActivityReconciliation.decide(local: Self.revision(sequence: 4), remote: Self.revision(sequence: 5),
                                                  hasPendingLocalMutation: false, sameScope: true) == .preserveUntilSync)
    }

    /// A queued local edit is not acknowledged state, whatever the cursor
    /// says: the card waits for the server's answer.
    @Test func aQueuedLocalMutationWaitsForTheServer() {
        #expect(LiveActivityReconciliation.decide(local: Self.revision(sequence: 9), remote: Self.revision(sequence: 5),
                                                  hasPendingLocalMutation: true, sameScope: true) == .preserveUntilSync)
    }

    @Test func theSameLifecycleAtOrPastTheCardIsApplied() {
        #expect(LiveActivityReconciliation.decide(local: Self.revision(sequence: 5), remote: Self.revision(sequence: 5),
                                                  hasPendingLocalMutation: false, sameScope: true) == .apply)
        #expect(LiveActivityReconciliation.decide(local: Self.revision(sequence: 8), remote: Self.revision(sequence: 5),
                                                  hasPendingLocalMutation: false, sameScope: true) == .apply)
    }

    @Test func anotherLifecycleAtOrPastTheCardIsAdopted() {
        #expect(LiveActivityReconciliation.decide(local: Self.revision(generation: Self.otherGeneration, sequence: 8),
                                                  remote: Self.revision(sequence: 5), hasPendingLocalMutation: false,
                                                  sameScope: true) == .adopt)
    }

    // MARK: winner

    typealias Candidate = LiveActivityReconciliation.Candidate

    /// The server sends to the smallest activity ID among the registered
    /// cards of the current lifecycle and ends the rest, so the phone keeps
    /// exactly that one — whichever receipt arrived first.
    @Test func registeredCardsOfTheCurrentLifecycleKeepTheServersPrimary() {
        let lifecycle = LiveActivityReconciliation.Lifecycle(timerID: Self.timer, sessionID: nil)
        let a = Candidate(id: "A-card", isServerKnown: true, generationID: Self.generation, timerID: Self.timer, sessionID: nil, remote: nil)
        let b = Candidate(id: "B-card", isServerKnown: true, generationID: Self.generation, timerID: Self.timer, sessionID: nil,
                          remote: Self.revision(sequence: 9, version: 4))
        for order in [[a, b], [b, a]] {
            #expect(LiveActivityReconciliation.winner(among: order, ownedID: "B-card", lifecycle: lifecycle) == "A-card")
        }
    }

    /// A registered card beats the card this phone requested but has not
    /// registered yet; with no registered card the owned one wins over an
    /// unregistered remote start.
    @Test func aRegisteredCardBeatsTheOwnedCardWhichBeatsAnUnregisteredRemoteStart() {
        let lifecycle = LiveActivityReconciliation.Lifecycle(timerID: Self.timer, sessionID: nil)
        let owned = Candidate(id: "Z-local", isServerKnown: false, generationID: nil, timerID: nil, sessionID: nil, remote: nil)
        let remote = Candidate(id: "A-remote", isServerKnown: false, generationID: Self.generation, timerID: Self.timer, sessionID: nil,
                               remote: Self.revision(sequence: 3))
        #expect(LiveActivityReconciliation.winner(among: [remote, owned], ownedID: "Z-local", lifecycle: lifecycle) == "Z-local")
        let registered = Candidate(id: "Y-remote", isServerKnown: true, generationID: Self.generation, timerID: Self.timer, sessionID: nil,
                                   remote: Self.revision(sequence: 3))
        #expect(LiveActivityReconciliation.winner(among: [owned, registered, remote], ownedID: "Z-local", lifecycle: lifecycle) == "Y-remote")
    }

    /// A registered card left over from a finished lifecycle never outranks
    /// the current one, even with the smaller ID.
    @Test func aRegisteredCardOfAFinishedLifecycleLoses() {
        let lifecycle = LiveActivityReconciliation.Lifecycle(timerID: Self.timer, sessionID: nil)
        let old = Candidate(id: "A-old", isServerKnown: true, generationID: Self.otherGeneration, timerID: UUID(), sessionID: nil,
                            remote: Self.revision(generation: Self.otherGeneration, sequence: 2))
        let current = Candidate(id: "B-current", isServerKnown: true, generationID: Self.generation, timerID: Self.timer, sessionID: nil,
                                remote: nil)
        #expect(LiveActivityReconciliation.winner(among: [old, current], ownedID: nil, lifecycle: lifecycle) == "B-current")
        #expect(LiveActivityReconciliation.winner(among: [], ownedID: nil, lifecycle: lifecycle) == nil)
    }

    // MARK: revision reader

    /// The local revision is the durable applied cursor and nothing more: a
    /// queued local start is reported as pending, never folded into the
    /// sequence; a cursor from another epoch vouches for nothing.
    @Test func theReaderNeverManufacturesAnAcknowledgedCursor() throws {
        let store = try BabyStore.sharedFixture(dateProvider: TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000)))
        let fence = LiveActivityRealmFence(serverInstanceID: Self.instance, recoveryEpoch: 2, familyID: Self.family)
        let reader = LiveActivityRevisionReader(store: store)
        #expect(reader.appliedCursor(in: fence) == nil)
        #expect(reader.localRevision(scope: Self.scope(), remote: Self.revision(sequence: 3),
                                     lifecycle: .init(timerID: Self.timer, sessionID: nil), registeredGeneration: nil) == nil)

        try store.recordCursor(Cursor(serverInstanceID: Self.instance, recoveryEpoch: 2, familyID: Self.family, sequence: 7))
        try store.context.save()
        let timer = try store.startPart(.breast(.left), for: Fixture.babyID)
        #expect(reader.hasPendingMutation(babyID: Fixture.babyID))
        #expect(!reader.hasPendingMutation(babyID: UUID()))

        let local = try #require(reader.localRevision(scope: Self.scope(), remote: Self.revision(timer: timer.id, sequence: 3, version: 6),
                                                       lifecycle: .init(timerID: timer.id, sessionID: nil), registeredGeneration: nil))
        #expect(local.familySequence == 7)
        #expect(local.generationID == Self.generation)
        #expect(local.timerID == timer.id)
        #expect(local.presentationVersion == 6)
        #expect(local.presentationTimestamp == 1_800_000_000)

        // Another epoch's cursor is not this realm's acknowledgement.
        let nextEpoch = LiveActivityRealmFence(serverInstanceID: Self.instance, recoveryEpoch: 3, familyID: Self.family)
        #expect(reader.appliedCursor(in: nextEpoch) == nil)
    }

    @Test func acknowledgedMutationsAreNoLongerPending() throws {
        let store = try BabyStore.sharedFixture(dateProvider: TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000)))
        let reader = LiveActivityRevisionReader(store: store)
        _ = try store.startSleep(for: Fixture.babyID)
        #expect(reader.hasPendingMutation(babyID: Fixture.babyID))
        try store.acknowledgeAllForTesting()
        #expect(!reader.hasPendingMutation(babyID: Fixture.babyID))
    }

    @Test func theFenceBelongsToOneSharedRealm() {
        let fence = LiveActivityRealmFence(serverInstanceID: Self.instance, recoveryEpoch: 2, familyID: Self.family)
        #expect(fence.matches(Fixture.sharedRealm))
        #expect(!fence.matches(.local))
        #expect(!fence.matches(.shared(canonicalOrigin: "https://sync.example", serverInstanceID: Self.instance, familyID: UUID())))
        #expect(fence.contains(Self.scope()))
        #expect(!fence.contains(Self.scope(epoch: 3)))
        #expect(fence.scope(babyID: Fixture.babyID, kindRaw: "sleep")?.kindRaw == "sleep")
        #expect(fence.scope(babyID: Fixture.babyID, kindRaw: "diaper") == nil)
    }
}

@MainActor
extension BabyStore {
    /// Every queued mutation acknowledged, as a successful push leaves them.
    func acknowledgeAllForTesting() throws {
        for row in try context.fetch(FetchDescriptor<SyncOutboxEntry>()) where !row.deliveryState.isTerminal {
            row.deliveryState = .acknowledged
        }
        try context.save()
    }
}
