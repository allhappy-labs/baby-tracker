import Foundation
import SwiftData
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

/// Test-only reach-ins. None of these are product API: they exist so a test can
/// set up or inspect durable state that production code reaches only through a
/// server round trip.
@MainActor
extension BabyStore {
    /// Switches the active realm the way a completed import would — moving
    /// the local rows across with it.
    func activateRealmForTesting(_ realm: RealmID) throws {
        let checkpoint = UUID()
        try beginTransition(to: realm, checkpointID: checkpoint)
        try commitTransition(checkpointID: checkpoint)
    }

    /// Points at a realm without moving anything, the way a device that
    /// already holds shared rows from sync does.
    func pointAtRealmForTesting(_ realm: RealmID) throws {
        try setActiveRealm(realm)
    }

    /// Drops rows created while seeding, so a test starts from an empty outbox.
    func clearOutboxForTesting() throws {
        for row in try context.fetch(FetchDescriptor<SyncOutboxEntry>()) { context.delete(row) }
        try context.save()
    }

    func activeTimerForTesting(_ id: UUID) -> ActiveTimer? {
        try? activeTimer(id: id)
    }

    func feedEntryForTesting(_ id: UUID) throws -> FeedEntry? {
        try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate { $0.id == id })).first
    }

    func sleepEntryForTesting(_ id: UUID) throws -> SleepEntry? {
        try context.fetch(FetchDescriptor<SleepEntry>(predicate: #Predicate { $0.id == id })).first
    }

    func diaperEntryForTesting(_ id: UUID) throws -> DiaperEntry? {
        try context.fetch(FetchDescriptor<DiaperEntry>(predicate: #Predicate { $0.id == id })).first
    }

    func diaperCountForTesting() throws -> Int {
        try context.fetch(FetchDescriptor<DiaperEntry>()).count
    }

    func supersedeForTesting(_ mutationID: UUID, code: String) throws {
        let row = try outboxEntry(mutationID)
        row.deliveryState = .superseded
        row.supersededCode = code
        try context.save()
    }

    func acknowledgeForTesting(_ mutationID: UUID) throws {
        let row = try outboxEntry(mutationID)
        row.deliveryState = .acknowledged
        try context.save()
    }

    /// Drives the rollback path: a payload `JSONSerialization` cannot represent
    /// must leave neither the entity nor an outbox row behind.
    func logDiaperFailingEncodeForTesting(for babyID: UUID) throws -> DiaperEntry {
        let expected = try captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let entry = DiaperEntry(
                babyID: babyID,
                contents: .wet,
                startedAt: self.now,
                timeZoneID: TimeZone.current.identifier,
                createdBy: self.deviceID
            )
            // A non-finite double is exactly what `payloadFields()` cannot encode.
            entry.color = nil
            self.context.insert(entry)
            try self.recordLocked(.create, on: UnencodableEntity(entityID: entry.id))
            return entry
        }
    }
}

/// A `ChangeLoggable` whose payload cannot be serialised.
private struct UnencodableEntity: ChangeLoggable {
    let entityID: UUID
    var entityTypeName: String { "DiaperEntry" }
    func payloadFields() -> [String: Any] { ["broken": Double.infinity] }
}
