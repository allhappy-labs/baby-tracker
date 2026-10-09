import Foundation
import SwiftData

@Model
public final class AccountAccessState {
    @Attribute(.unique) public var key: String = ""
    public var allowsOrdinarySync: Bool = false
    public var boundRealmKey: String?
    public var credentialVersion: UUID?
    public var refreshStateData: Data = Data()

    public init(key: AccountLeaseKey, allowsOrdinarySync: Bool = false) {
        self.key = key.encodedKey
        self.allowsOrdinarySync = allowsOrdinarySync
        self.refreshStateData = (try? JSONEncoder().encode(RefreshAttemptState.idle)) ?? Data()
    }

    /// Kept in the existing encoded payload so V2–V7 model hashes stay frozen.
    public var refreshRecoveryData: Data? {
        get throws { try recoveryPayload().recovery }
    }

    func recoveryPayload() throws -> (state: RefreshAttemptState, recovery: Data?) {
        do {
            let state = try JSONDecoder().decode(RefreshAttemptState.self, from: refreshStateData)
            guard let object = try JSONSerialization.jsonObject(with: refreshStateData) as? [String: Any] else { throw RefreshRecoveryError.corruptCheckpoint }
            guard let value = object["refreshRecoveryData"] else { return (state, nil) }
            guard let encoded = value as? String, let data = Data(base64Encoded: encoded) else { throw RefreshRecoveryError.corruptCheckpoint }
            return (state, data)
        } catch { throw RefreshRecoveryError.corruptCheckpoint }
    }

    func setRecoveryPayload(state: RefreshAttemptState, recovery: Data?) throws {
        let encoded = try JSONEncoder().encode(state)
        guard var object = try JSONSerialization.jsonObject(with: encoded) as? [String: Any] else { throw RefreshRecoveryError.corruptCheckpoint }
        if let recovery { object["refreshRecoveryData"] = recovery.base64EncodedString() }
        refreshStateData = try JSONSerialization.data(withJSONObject: object, options: .sortedKeys)
    }

    public var refreshState: RefreshAttemptState {
        get { (try? JSONDecoder().decode(RefreshAttemptState.self, from: refreshStateData)) ?? .idle }
        set {
            // Legacy callers preserve recovery metadata. Corrupt bytes are never
            // replaced with an idle value; recovery mutations use throwing APIs.
            guard let payload = try? recoveryPayload() else { return }
            try? setRecoveryPayload(state: newValue, recovery: payload.recovery)
        }
    }
}
