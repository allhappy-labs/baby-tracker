import BabyStore
import Foundation

/// The non-secret half of a family session: enough to know, before the
/// Keychain is even asked, which endpoint and family a relaunch should try to
/// resume. Kept in the App Group's `UserDefaults` rather than the Keychain
/// because none of it is a credential — the actual tokens stay Keychain-only,
/// looked up by `familyKey` once this record says where to look.
public struct FamilySessionRecord: Codable, Sendable, Equatable {
    public let endpoint: ConfirmedEndpoint
    public let familyKey: FamilySessionKey

    public init(endpoint: ConfirmedEndpoint, familyKey: FamilySessionKey) {
        self.endpoint = endpoint
        self.familyKey = familyKey
    }

    /// The record the app last saved, read-only: for a process that acts for
    /// the family without restoring the whole account — an App Intent behind
    /// a Lock Screen button. Nil when no family session is saved.
    public static func saved(in defaults: UserDefaults? = nil) -> FamilySessionRecord? {
        FamilySessionRecordStore.load(from: defaults ?? UserDefaults(suiteName: BabySchema.appGroupID) ?? .standard)
    }
}

/// Reads and writes the single durable record under one pinned key. A
/// dedicated type rather than inline `UserDefaults` calls in
/// `AccountCoordinator`, so every save/clear site agrees on the same
/// encoding and key.
enum FamilySessionRecordStore {
    static let defaultsKey = "familySync.session.v1"

    static func save(_ record: FamilySessionRecord, to defaults: UserDefaults) {
        guard let data = try? JSONEncoder().encode(record) else { return }
        defaults.set(data, forKey: defaultsKey)
    }

    static func load(from defaults: UserDefaults) -> FamilySessionRecord? {
        guard let data = defaults.data(forKey: defaultsKey) else { return nil }
        return try? JSONDecoder().decode(FamilySessionRecord.self, from: data)
    }

    static func clear(from defaults: UserDefaults) {
        defaults.removeObject(forKey: defaultsKey)
    }
}
