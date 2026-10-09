import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor
@Test func aFailedRecordStillRollsBackAndRethrows() throws {
    // Logging must not change behaviour: the rollback contract from v1 holds.
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = Baby(name: "Mira", birthDate: .now)
    store.context.insert(baby)

    #expect(throws: (any Error).self) {
        try store.record(.create, on: UnloggableProbe())
    }
    #expect(store.context.hasChanges == false)
    #expect(try store.changeLog().isEmpty)
}

/// Payload that `JSONSerialization` rejects, to drive the failure path.
private struct UnloggableProbe: ChangeLoggable {
    let entityID = UUID()
    var entityTypeName: String { "UnloggableProbe" }
    func payloadFields() -> [String: Any] { ["when": Double.nan] }
}

@MainActor
@Test func loggingDoesNotAlterTheChangeLog() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    _ = try store.logDiaper(.wet, for: baby.id)

    #expect(try store.changeLog().count == 3)
}
