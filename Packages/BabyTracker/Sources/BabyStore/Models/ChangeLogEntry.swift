import Foundation
import SwiftData

public enum ChangeOperation: String, Codable, Sendable, CaseIterable {
    case create, update, delete
}

/// Append-only outbox. Nothing drains it in v1; the future sync node will.
@Model
public final class ChangeLogEntry {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var entityID: UUID = UUID()
    public var entityType: String = ""
    public var operationRaw: String = ChangeOperation.create.rawValue
    public var payload: Data = Data()
    public var timestamp: Date = Date()
    public var deviceID: String = ""
    public var syncedAt: Date?

    public init(
        entityID: UUID,
        entityType: String,
        operation: ChangeOperation,
        payload: Data,
        timestamp: Date,
        deviceID: String,
        realm: RealmID = .local
    ) {
        self.entityID = entityID
        self.realmKey = realm.encodedKey
        self.entityType = entityType
        self.operationRaw = operation.rawValue
        self.payload = payload
        self.timestamp = timestamp
        self.deviceID = deviceID
    }

    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }

    public var operation: ChangeOperation {
        get { ChangeOperation(rawValue: operationRaw) ?? .create }
        set { operationRaw = newValue.rawValue }
    }
}
