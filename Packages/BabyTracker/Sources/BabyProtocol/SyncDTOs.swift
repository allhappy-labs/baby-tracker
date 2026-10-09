import Foundation

public struct Outcome: Codable, Sendable, Equatable {
    public let mutationID: UUID
    public let status: OutcomeStatus
    public let originalStatus: OutcomeStatus?
    public let canonicalEntity: CanonicalEntity?
    public let relatedEntities: [CanonicalEntity]
    public let cursor: Cursor?
    public let code: String?
    public let overwroteNewerVersion: Bool

    enum CodingKeys: String, CodingKey {
        case mutationID = "mutation_id", status, originalStatus = "original_status"
        case canonicalEntity = "canonical_entity", relatedEntities = "related_entities", cursor, code
        case overwroteNewerVersion = "overwrote_newer_version"
    }

    /// `already_applied` is only interpretable together with the status the
    /// server recorded the first time. A receipt that omits it is unstable and
    /// must not be allowed to release or supersede anything.
    public var requiredOriginalStatus: OutcomeStatus {
        get throws {
            guard status == .alreadyApplied else { return status }
            guard let originalStatus, originalStatus != .alreadyApplied else {
                throw OutcomeError.unstableReceipt(mutationID)
            }
            return originalStatus
        }
    }
}

public enum OutcomeError: Error, Sendable, Equatable {
    case unstableReceipt(UUID)
}

public struct JournalChange: Codable, Sendable, Equatable {
    public let cursor: Cursor
    public let entityKind: String
    public let entityID: UUID
    public let entityVersion: Int64
    public let actorAccountID: UUID
    public let deviceID: UUID
    public let operation: String
    public let acceptedAt: String
    public let optional: Bool
    public let snapshotOrTombstone: CanonicalEntity

    enum CodingKeys: String, CodingKey {
        case cursor, entityKind = "entity_kind", entityID = "entity_id", entityVersion = "entity_version"
        case actorAccountID = "actor_account_id", deviceID = "device_id", operation, acceptedAt = "accepted_at"
        case optional, snapshotOrTombstone = "snapshot_or_tombstone"
    }
}

public struct SyncResponse: Codable, Sendable, Equatable {
    public init(
        serverInstanceID: UUID, recoveryEpoch: Int64, outcomes: [Outcome],
        changes: [JournalChange], highWater: Cursor, nextPageToken: String?
    ) {
        self.serverInstanceID = serverInstanceID
        self.recoveryEpoch = recoveryEpoch
        self.outcomes = outcomes
        self.changes = changes
        self.highWater = highWater
        self.nextPageToken = nextPageToken
    }

    public let serverInstanceID: UUID
    public let recoveryEpoch: Int64
    public let outcomes: [Outcome]
    public let changes: [JournalChange]
    public let highWater: Cursor
    public let nextPageToken: String?

    enum CodingKeys: String, CodingKey {
        case serverInstanceID = "server_instance_id", recoveryEpoch = "recovery_epoch"
        case outcomes, changes, highWater = "high_water", nextPageToken = "next_page_token"
    }
}

public struct SnapshotResponse: Codable, Sendable, Equatable {
    public init(
        serverInstanceID: UUID, recoveryEpoch: Int64, snapshotID: UUID,
        highWater: Cursor, entities: [CanonicalEntity], nextPageToken: String?
    ) {
        self.serverInstanceID = serverInstanceID
        self.recoveryEpoch = recoveryEpoch
        self.snapshotID = snapshotID
        self.highWater = highWater
        self.entities = entities
        self.nextPageToken = nextPageToken
    }

    public let serverInstanceID: UUID
    public let recoveryEpoch: Int64
    public let snapshotID: UUID
    public let highWater: Cursor
    public let entities: [CanonicalEntity]
    public let nextPageToken: String?

    enum CodingKeys: String, CodingKey {
        case serverInstanceID = "server_instance_id", recoveryEpoch = "recovery_epoch"
        case snapshotID = "snapshot_id", highWater = "high_water", entities, nextPageToken = "next_page_token"
    }
}
