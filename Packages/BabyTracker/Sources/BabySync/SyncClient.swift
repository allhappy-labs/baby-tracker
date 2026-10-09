import Foundation

/// The wire shape. Deliberately free of SwiftData and of BabyStore's types, so
/// the future self-hosted node can be written against this alone.
public struct PendingChange: Codable, Sendable, Equatable, Identifiable {
    public let id: UUID
    public let entityID: UUID
    public let entityType: String
    public let operation: String
    public let payload: Data
    public let timestamp: Date
    public let deviceID: String

    public init(
        id: UUID = UUID(),
        entityID: UUID,
        entityType: String,
        operation: String,
        payload: Data,
        timestamp: Date,
        deviceID: String
    ) {
        self.id = id
        self.entityID = entityID
        self.entityType = entityType
        self.operation = operation
        self.payload = payload
        self.timestamp = timestamp
        self.deviceID = deviceID
    }
}

public protocol SyncClient: Sendable {
    /// Returns the ids the server durably accepted. Anything absent stays pending.
    func push(_ changes: [PendingChange]) async throws -> [UUID]

    /// Changes from other devices since the given watermark.
    func pull(since: Date?) async throws -> [PendingChange]
}
