import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@MainActor
@Test func everyMutationAppendsExactlyOneChangeLogEntry() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)

    let baby = Baby(name: "Mira", birthDate: .now)
    store.context.insert(baby)
    try store.record(.create, on: baby)

    let log = try store.changeLog()
    #expect(log.count == 1)
    #expect(log[0].entityID == baby.id)
    #expect(log[0].entityType == "Baby")
    #expect(log[0].operation == .create)
    #expect(log[0].timestamp == clock.now)
}

@MainActor
@Test func changeLogPayloadCarriesTheEntitysFields() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))

    let feed = FeedEntry(babyID: UUID(), kind: .bottle, startedAt: .now, timeZoneID: "UTC")
    feed.components = [.bottle(id: UUID(), volumeML: 120, contents: .formula)]
    store.context.insert(feed)
    try store.record(.create, on: feed)

    let payload = try store.changeLog()[0].payload
    let decoded = try JSONSerialization.jsonObject(with: payload) as! [String: Any]

    #expect(decoded["baby_id"] as? String == feed.babyID.uuidString.lowercased())
    #expect(decoded["kind"] as? String == "bottle")
    let components = try #require(decoded["components"] as? [[String: Any]])
    #expect(components.count == 1)
    let component = try #require(components.first)
    let storedComponent = try #require(feed.components.first)
    #expect(component["id"] as? String == storedComponent.id.uuidString.lowercased())
    #expect(component["kind"] as? String == "bottle")
    #expect(component["volume_ml"] as? Double == 120)
    #expect(component["contents"] as? String == "formula")
}

@MainActor
@Test func operationsAccumulateInOrder() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)

    let baby = Baby(name: "Mira", birthDate: .now)
    store.context.insert(baby)
    try store.record(.create, on: baby)

    clock.advance(by: 60)
    baby.name = "Mira R"
    try store.record(.update, on: baby)

    clock.advance(by: 60)
    try store.record(.delete, on: baby)

    let log = try store.changeLog()
    #expect(log.map(\.operation) == [.create, .update, .delete])
    #expect(log[2].timestamp.timeIntervalSince(log[0].timestamp) == 120)
}

@MainActor
@Test func changeLogStampsTheOriginatingDevice() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now), deviceID: "device-abc")

    let baby = Baby(name: "Mira", birthDate: .now)
    store.context.insert(baby)
    try store.record(.create, on: baby)

    #expect(try store.changeLog()[0].deviceID == "device-abc")
}

/// Test-only entity whose payload cannot be encoded as JSON, so `record`
/// fails inside `payloadData()` before the change-log entry is even built.
private struct UnencodableEntity: ChangeLoggable {
    let entityID = UUID()
    let entityTypeName = "Unencodable"

    func payloadFields() -> [String: Any] {
        // A non-finite Double is not representable in JSON, so
        // JSONSerialization.data(withJSONObject:) throws.
        ["when": Double.nan]
    }
}

@MainActor
@Test func recordRollsBackTheContextWhenPayloadEncodingFails() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))

    let baby = Baby(name: "Mira", birthDate: .now)
    store.context.insert(baby)

    #expect(throws: (any Error).self) {
        try store.record(.create, on: UnencodableEntity())
    }

    #expect(store.context.hasChanges == false)
    #expect(try store.changeLog().isEmpty)
    #expect(try store.context.fetch(FetchDescriptor<Baby>()).isEmpty)
}
