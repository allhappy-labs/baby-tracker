import BabyStore
import BabyCore
import Foundation

/// Keeps this phone's own copy of each invitation's shareable link.
///
/// The server answers with a token or link exactly once — a replayed create
/// or a later list read both say `secretUnavailable` rather than repeat the
/// secret — so the only way to show it again, or hand it to a share sheet a
/// second time, is a copy this device already kept. Scoped by `RealmID` —
/// the same identity `BabyStore` uses to keep one family's data from another
/// — so a link minted for one family never surfaces while a caregiver is
/// looking at a different one. Keyed off `RealmID.encodedKey`, the same
/// derivation `RealmWritePermission` uses, rather than a raw family id: the
/// local realm has no family id at all, and this store must still be able to
/// name a slot for it without special-casing that case itself.
public struct InvitationCodeStore: Sendable {
    private let backend: any KeychainBackend
    private let realm: RealmID

    public init(backend: any KeychainBackend, realm: RealmID) {
        self.backend = backend
        self.realm = realm
    }

    private static let service = AppIdentity.identifier("invitationCodes")

    public func save(_ shareURL: URL, for id: UUID) throws {
        try backend.write(Data(shareURL.absoluteString.utf8), service: Self.service, account: itemAccount(for: id))
        var ids = try index()
        if ids.insert(id).inserted {
            try writeIndex(ids)
        }
    }

    public func shareURL(for id: UUID) -> URL? {
        guard let data = try? backend.read(service: Self.service, account: itemAccount(for: id)),
              let string = String(data: data, encoding: .utf8) else { return nil }
        return URL(string: string)
    }

    public func remove(_ id: UUID) throws {
        try backend.delete(service: Self.service, account: itemAccount(for: id))
        var ids = try index()
        if ids.remove(id) != nil {
            try writeIndex(ids)
        }
    }

    /// Drops every kept code whose invitation is not in `ids` — a revoked or
    /// expired invitation the server no longer lists, say — without the
    /// caller having to know which ones those are.
    public func prune(keeping ids: Set<UUID>) throws {
        let current = try index()
        for stale in current.subtracting(ids) {
            try backend.delete(service: Self.service, account: itemAccount(for: stale))
        }
        try writeIndex(current.intersection(ids))
    }

    public func removeAll() throws {
        for id in try index() {
            try backend.delete(service: Self.service, account: itemAccount(for: id))
        }
        try backend.delete(service: Self.service, account: indexAccount)
    }

    // MARK: Plumbing

    private func itemAccount(for id: UUID) -> String {
        "code|\(realm.encodedKey)|\(id.uuidString.lowercased())"
    }

    /// The realm's own list of ids it has kept a code for. Kept as an index
    /// item rather than found by enumeration: a plain `KeychainBackend` has
    /// no way to list its own accounts, only `EnumeratingKeychainBackend`
    /// does, and this store works against either.
    private var indexAccount: String { "index|\(realm.encodedKey)" }

    private func index() throws -> Set<UUID> {
        guard let data = try backend.read(service: Self.service, account: indexAccount) else { return [] }
        return Set(try JSONDecoder().decode([UUID].self, from: data))
    }

    private func writeIndex(_ ids: Set<UUID>) throws {
        if ids.isEmpty {
            try backend.delete(service: Self.service, account: indexAccount)
        } else {
            try backend.write(try JSONEncoder().encode(Array(ids)), service: Self.service, account: indexAccount)
        }
    }
}
