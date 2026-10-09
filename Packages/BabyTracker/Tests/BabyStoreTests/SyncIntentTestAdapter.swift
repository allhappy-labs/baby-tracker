import Foundation
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

/// Shared identities and the stable P2 receipts the timer tests replay.
enum Fixture {
    static let sharedRealm = RealmID.shared(
        canonicalOrigin: "https://sync.example",
        serverInstanceID: UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!,
        familyID: UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
    )
    static let babyID = UUID(uuidString: "10000000-0000-4000-8000-000000000001")!

    /// Decodes a genuine `already_applied` envelope rather than constructing an
    /// `Outcome` in Swift, so the test exercises the wire shape the server
    /// actually sends.
    static func alreadyApplied(_ mutationID: UUID, originalStatus: String) throws -> Outcome {
        let json = """
        {
          "mutation_id": "\(mutationID.uuidString.lowercased())",
          "status": "already_applied",
          "original_status": "\(originalStatus)",
          "canonical_entity": null,
          "related_entities": [],
          "cursor": null,
          "code": null,
          "overwrote_newer_version": false
        }
        """
        return try JSONDecoder().decode(Outcome.self, from: Data(json.utf8))
    }
}

extension BabyStore {
    /// A store whose active realm is shared, so writes produce outbox intents.
    static func sharedFixture(dateProvider: DateProvider = TestDateProvider(.now)) throws -> BabyStore {
        let store = try BabyStore.inMemory(dateProvider: dateProvider)
        try store.activateRealmForTesting(Fixture.sharedRealm)
        let baby = Baby(id: Fixture.babyID, name: "Mira", birthDate: .now, realm: Fixture.sharedRealm)
        store.context.insert(baby)
        try store.context.save()
        try store.clearOutboxForTesting()
        return store
    }
}

/// Test-only sugar over the existing production timer APIs. It never creates an
/// outbox row, never manufactures a receipt, and never infers a mutation ID
/// from a domain ID — it looks the persisted row up and requires it to be
/// unique, so a reducer that wrote two rows fails the test rather than hiding.
@MainActor
struct SyncIntentTestAdapter {
    struct StartedFeed { let timerID: UUID; let mutationID: UUID }
    struct StoppedFeed { let recordID: UUID; let mutationID: UUID }

    let store: BabyStore

    func startFeed(side: BreastSide, for babyID: UUID) throws -> StartedFeed {
        let timer = try store.startPart(.breast(side), for: babyID)
        return StartedFeed(timerID: timer.id, mutationID: try uniqueMutationID(entityID: timer.id, operation: .timerStart))
    }

    func stopFeed(timerID: UUID) throws -> StoppedFeed {
        let timer = try #require(store.activeTimerForTesting(timerID))
        let entry = try #require(try store.stopFeed(timer))
        // A wholly undispatched lifecycle compacts into one `create feed`; an
        // uncertain one leaves the start in place and appends `timer_stop`.
        if let compacted = try? uniqueMutationID(entityID: entry.id, operation: .create) {
            return StoppedFeed(recordID: entry.id, mutationID: compacted)
        }
        return StoppedFeed(recordID: entry.id, mutationID: try uniqueMutationID(entityID: timerID, operation: .timerStop))
    }

    private func uniqueMutationID(entityID: UUID, operation: WireOperation) throws -> UUID {
        let matches = try store.pendingOutbox().filter { $0.entityID == entityID && $0.operation == operation }
        guard matches.count == 1, let only = matches.first else {
            throw AdapterError.notUnique(entityID: entityID, operation: operation.rawValue, count: matches.count)
        }
        return only.id
    }

    enum AdapterError: Error, CustomStringConvertible {
        case notUnique(entityID: UUID, operation: String, count: Int)
        var description: String {
            switch self {
            case .notUnique(let entityID, let operation, let count):
                "expected exactly one \(operation) outbox row for \(entityID), found \(count)"
            }
        }
    }
}
