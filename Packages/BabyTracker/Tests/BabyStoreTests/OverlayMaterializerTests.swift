import Foundation
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Overlay materialization")
struct OverlayMaterializerTests {
    @Test func overlayOrdersOnlyLiveMutationsForItsEntity() throws {
        let store = try BabyStore.sharedFixture()
        let first = try store.logDiaper(.wet, for: Fixture.babyID)
        let second = try store.logDiaper(.dirty, for: Fixture.babyID)

        let overlay = try store.overlay(for: first.id, kind: .diaper)
        #expect(overlay.entityID == first.id)
        #expect(overlay.entityKind == .diaper)
        #expect(overlay.orderedMutationIDs.count == 1)
        #expect(try store.overlay(for: second.id, kind: .diaper).orderedMutationIDs.count == 1)
    }

    @Test func supersededMutationsLeaveTheOverlay() throws {
        let store = try BabyStore.sharedFixture()
        let entry = try store.logDiaper(.wet, for: Fixture.babyID)
        let mutationID = try #require(try store.pendingOutbox().first { $0.entityID == entry.id }).id

        #expect(try store.overlay(for: entry.id, kind: .diaper).orderedMutationIDs == [mutationID])
        try store.supersedeForTesting(mutationID, code: "entity_deleted")
        #expect(try store.overlay(for: entry.id, kind: .diaper).orderedMutationIDs.isEmpty)
    }

    @Test func baseIsReplacedNotAccumulatedWhenApplied() throws {
        let store = try BabyStore.sharedFixture()
        let entry = try store.logDiaper(.wet, for: Fixture.babyID)
        let mutationID = try #require(try store.pendingOutbox().first { $0.entityID == entry.id }).id

        try store.markDispatched(mutationIDs: [mutationID])
        try store.acknowledgeForTesting(mutationID)

        #expect(try store.overlay(for: entry.id, kind: .diaper).orderedMutationIDs.isEmpty)
        #expect(try store.nextDispatchBatch(limit: 10).isEmpty)
    }
}
