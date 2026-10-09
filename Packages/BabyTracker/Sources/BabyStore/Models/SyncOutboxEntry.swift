import CryptoKit
import Foundation
import SwiftData
import BabyProtocol

/// How far a mutation has got towards the server.
///
/// The distinction that matters is `inFlightOrUncertain`: once bytes may have
/// left the device, the payload is frozen and the mutation ID is fixed
/// forever, because the server may have already applied it under that ID.
/// Editing either would make a retry a *different* mutation and apply the same
/// intent twice.
public enum MutationDeliveryState: String, Codable, Sendable {
    case neverDispatched, inFlightOrUncertain, acknowledged, quarantined, superseded

    /// Terminal states are never dispatched again and never appear in an overlay.
    public var isTerminal: Bool {
        switch self {
        case .acknowledged, .quarantined, .superseded: true
        case .neverDispatched, .inFlightOrUncertain: false
        }
    }
}

@Model
public final class SyncOutboxEntry {
    /// The mutation ID. Once dispatched it is immutable.
    @Attribute(.unique) public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var entityID: UUID = UUID()
    public var entityKindRaw: String = ""
    public var operationRaw: String = ""
    /// The encoded `WireMutation`. Frozen when the row leaves `neverDispatched`.
    public var payload: Data = Data()
    /// SHA-256 of `payload`, frozen alongside it.
    public var payloadHash: Data = Data()
    public var deliveryStateRaw: String = MutationDeliveryState.neverDispatched.rawValue
    /// A command that may not be sent until this mutation is applied.
    public var dependencyMutationID: UUID?
    /// Why the server refused, for a terminally superseded row.
    public var supersededCode: String?
    /// Monotonic per-store ordering; dispatch order is submission order.
    public var sequence: Int64 = 0
    public var createdAt: Date = Date()

    public init(
        id: UUID = UUID(),
        realm: RealmID,
        entityID: UUID,
        entityKind: WireEntityKind,
        operation: WireOperation,
        payload: Data,
        sequence: Int64,
        createdAt: Date,
        dependencyMutationID: UUID? = nil
    ) {
        self.id = id
        realmKey = realm.encodedKey
        self.entityID = entityID
        entityKindRaw = entityKind.rawValue
        operationRaw = operation.rawValue
        self.payload = payload
        payloadHash = Data()
        self.sequence = sequence
        self.createdAt = createdAt
        self.dependencyMutationID = dependencyMutationID
    }

    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }
    public var entityKind: WireEntityKind { WireEntityKind(rawValue: entityKindRaw) ?? .baby }
    public var operation: WireOperation { WireOperation(rawValue: operationRaw) ?? .create }

    public var deliveryState: MutationDeliveryState {
        get { MutationDeliveryState(rawValue: deliveryStateRaw) ?? .neverDispatched }
        set { deliveryStateRaw = newValue.rawValue }
    }

    /// Replaces the payload. Refused once the bytes may have been sent.
    func replacePayload(_ data: Data) throws {
        guard deliveryState == .neverDispatched else { throw SyncPersistenceError.payloadFrozen(id) }
        payload = data
    }

    /// Freezes the hash exactly once, at the moment the row first becomes
    /// dispatchable-uncertain.
    func freezePayloadHashIfNeeded() {
        guard payloadHash.isEmpty else { return }
        payloadHash = Data(SHA256.hash(data: payload))
    }

    public func decodedMutation() throws -> WireMutation {
        try JSONDecoder().decode(WireMutation.self, from: payload)
    }

    /// The mutation, if this build can still send it. A row an older build
    /// queued in a shape the server no longer accepts — a feed at payload
    /// version 2, a timer at 4 — throws, like an operation this build no
    /// longer knows, and dispatch quarantines it rather than sending what
    /// can only be refused.
    public func dispatchableMutation() throws -> WireMutation {
        let mutation = try decodedMutation()
        guard mutation.declaresAcceptedPayloadVersion else {
            throw SyncPersistenceError.outdatedPayloadVersion(id)
        }
        return mutation
    }
}

public enum SyncPersistenceError: Error, Sendable, Equatable {
    case payloadFrozen(UUID)
    case outdatedPayloadVersion(UUID)
    case unknownMutation(UUID)
    case unstableTimerReceipt
}
