import Foundation

public enum RealmID: Codable, Sendable, Hashable {
    case local
    case shared(canonicalOrigin: String, serverInstanceID: UUID, familyID: UUID)

    public var encodedKey: String {
        switch self {
        case .local: return "local"
        case .shared(let origin, let instance, let family):
            return "shared|\(Data(origin.utf8).base64EncodedString())|\(instance.uuidString.lowercased())|\(family.uuidString.lowercased())"
        }
    }

    public init?(encodedKey: String) {
        if encodedKey == "local" { self = .local; return }
        let pieces = encodedKey.split(separator: "|", omittingEmptySubsequences: false)
        guard pieces.count == 4, pieces[0] == "shared",
              let originData = Data(base64Encoded: String(pieces[1])),
              let origin = String(data: originData, encoding: .utf8),
              let instance = UUID(uuidString: String(pieces[2])),
              let family = UUID(uuidString: String(pieces[3])) else { return nil }
        self = .shared(canonicalOrigin: origin, serverInstanceID: instance, familyID: family)
    }
}

public struct RealmWriteContext: Sendable, Equatable {
    public let realm: RealmID
    public let transitionGeneration: Int64
    public init(realm: RealmID, transitionGeneration: Int64) {
        self.realm = realm
        self.transitionGeneration = transitionGeneration
    }
}

public enum RealmWriteError: Error, Sendable, Equatable { case retryAfterTransition, wrongRealm }

@MainActor
public protocol RealmStore: AnyObject {
    var activeRealm: RealmID { get }
    func beginTransition(to realm: RealmID, checkpointID: UUID) throws
    func commitTransition(checkpointID: UUID) throws
    func recoverIncompleteTransition() throws
}
