import Foundation
import Testing
@testable import BabyCore
@testable import BabyStore

@MainActor
@Suite("Realm transitions")
struct RealmTransitionTests {
    @Test func interruptedTransitionNeverExposesMixedRealmRows() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        _ = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let target = RealmID.shared(
            canonicalOrigin: "https://sync.example",
            serverInstanceID: UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!,
            familyID: UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
        )

        try store.beginTransition(to: target, checkpointID: UUID(uuidString: "cccccccc-cccc-4ccc-8ccc-cccccccccccc")!)

        #expect(store.activeRealm == .local)
        #expect(try store.babies().allSatisfy { $0.realmID == .local })
    }

    @Test func staleAndWrongRealmWritesLeaveRowsAndGenerationUnchanged() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let captured = try store.captureWriteContext()
        try store.beginTransition(to: .shared(canonicalOrigin: "https://sync.example", serverInstanceID: UUID(), familyID: UUID()), checkpointID: UUID())

        #expect(throws: RealmWriteError.retryAfterTransition) {
            try store.withRealmWrite(expected: captured) { baby.name = "Changed" }
        }
        #expect(baby.name == "Mira")
        #expect(try store.captureWriteContext().transitionGeneration != captured.transitionGeneration)
    }
}
