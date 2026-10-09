import Testing
import Foundation
@testable import BabySync

@Test func localOnlyAcknowledgesEverythingAndReturnsNothing() async throws {
    let client = LocalOnlySync()
    let change = PendingChange(
        entityID: UUID(),
        entityType: "DiaperEntry",
        operation: "create",
        payload: Data("{}".utf8),
        timestamp: Date(timeIntervalSince1970: 1_700_000_000),
        deviceID: "device-abc"
    )

    let acknowledged = try await client.push([change])
    #expect(acknowledged == [change.id])

    let pulled = try await client.pull(since: nil)
    #expect(pulled.isEmpty)
}

@Test func pushingNothingIsNotAnError() async throws {
    let client = LocalOnlySync()
    #expect(try await client.push([]).isEmpty)
}

@Test func pendingChangeRoundTripsThroughJSON() throws {
    let change = PendingChange(
        entityID: UUID(),
        entityType: "FeedEntry",
        operation: "update",
        payload: Data(#"{"volumeML":120}"#.utf8),
        timestamp: Date(timeIntervalSince1970: 1_700_000_000),
        deviceID: "device-abc"
    )

    let encoded = try JSONEncoder().encode(change)
    let decoded = try JSONDecoder().decode(PendingChange.self, from: encoded)

    #expect(decoded == change)
}
