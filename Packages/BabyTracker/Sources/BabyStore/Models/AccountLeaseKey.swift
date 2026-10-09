import Foundation

public struct AccountLeaseKey: Codable, Sendable, Hashable {
    public let canonicalOrigin: String
    public let serverInstanceID: UUID
    public let accountID: UUID
    public let deviceID: UUID

    public init(canonicalOrigin: String, serverInstanceID: UUID, accountID: UUID, deviceID: UUID) {
        self.canonicalOrigin = canonicalOrigin
        self.serverInstanceID = serverInstanceID
        self.accountID = accountID
        self.deviceID = deviceID
    }

    public var encodedKey: String {
        "\(Data(canonicalOrigin.utf8).base64EncodedString())|\(serverInstanceID.uuidString.lowercased())|\(accountID.uuidString.lowercased())|\(deviceID.uuidString.lowercased())"
    }
}

public enum LeaseScope: Codable, Sendable, Hashable {
    case realm(RealmID)
    case account(AccountLeaseKey)

    public var encodedKey: String {
        switch self {
        case .realm(let realm): "realm|\(realm.encodedKey)"
        case .account(let account): "account|\(account.encodedKey)"
        }
    }
}

public struct FencedLeaseToken: Codable, Sendable, Hashable {
    public let scope: LeaseScope
    public let ownerID: UUID
    public let generation: Int64
    public let expiresAt: Date
    public init(scope: LeaseScope, ownerID: UUID, generation: Int64, expiresAt: Date) {
        self.scope = scope; self.ownerID = ownerID; self.generation = generation; self.expiresAt = expiresAt
    }
}

public enum FencedLeaseError: Error, Sendable, Equatable { case staleOwner, expired, cancelled }

@MainActor
public protocol FencedLeaseStore: AnyObject {
    func acquire(scope: LeaseScope, ownerID: UUID, now: Date, ttl: TimeInterval) throws -> FencedLeaseToken?
    func renew(_ token: FencedLeaseToken, now: Date, ttl: TimeInterval) throws -> FencedLeaseToken
    func withFencedCommit<T>(_ token: FencedLeaseToken, now: Date, _ body: () throws -> T) throws -> T
    func cancel(_ token: FencedLeaseToken) throws
}
