import Foundation
import BabyCore
import BabyStore
import Security

// MARK: - Keys

/// Identity of a session that exists before any family does.
public struct AccountSessionKey: Codable, Sendable, Hashable {
    public let canonicalOrigin: String
    public let serverInstanceID: UUID
    public let accountID: UUID
    public let deviceID: UUID
    public let recoveryEpoch: UInt64

    public init(canonicalOrigin: String, serverInstanceID: UUID, accountID: UUID, deviceID: UUID, recoveryEpoch: UInt64) {
        self.canonicalOrigin = canonicalOrigin
        self.serverInstanceID = serverInstanceID
        self.accountID = accountID
        self.deviceID = deviceID
        self.recoveryEpoch = recoveryEpoch
    }
}

/// Identity of a session bound to one family.
public struct FamilySessionKey: Codable, Sendable, Hashable {
    public let canonicalOrigin: String
    public let serverInstanceID: UUID
    public let familyID: UUID
    public let accountID: UUID
    public let deviceID: UUID
    public let recoveryEpoch: UInt64

    public init(canonicalOrigin: String, serverInstanceID: UUID, familyID: UUID, accountID: UUID, deviceID: UUID, recoveryEpoch: UInt64) {
        self.canonicalOrigin = canonicalOrigin
        self.serverInstanceID = serverInstanceID
        self.familyID = familyID
        self.accountID = accountID
        self.deviceID = deviceID
        self.recoveryEpoch = recoveryEpoch
    }
}

/// Identity of the device-continuity proof. Deliberately epoch-free: the proof
/// outlives token epochs, which is the whole point of it.
public struct ContinuityKey: Codable, Sendable, Hashable {
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
}

// MARK: - Values

public struct DeviceContinuity: Codable, Sendable, Equatable {
    public let deviceID: UUID
    public let credential: String
    public init(deviceID: UUID, credential: String) {
        self.deviceID = deviceID
        self.credential = credential
    }
}

/// A continuity proof is always account-qualified, so a proof minted for one
/// account can never be offered as another account's claim on that device.
public struct DeviceContinuityBinding: Codable, Sendable, Equatable {
    public let accountID: UUID
    public let continuity: DeviceContinuity
    public init(accountID: UUID, continuity: DeviceContinuity) {
        self.accountID = accountID
        self.continuity = continuity
    }
}

public struct SessionCredentials: Codable, Sendable, Equatable {
    public let accessToken: String
    public let refreshToken: String
    public let expiresAt: Date
    public let recoveryEpoch: UInt64
    public let deviceContinuity: DeviceContinuity
    /// Client-generated and non-secret: it is how P3.1's account access row
    /// knows which install of the credentials it is looking at. Not a wire
    /// field and not a token digest.
    public let credentialVersion: UUID
    public let installBinding: RefreshInstallBinding?

    public init(
        accessToken: String,
        refreshToken: String,
        expiresAt: Date,
        recoveryEpoch: UInt64,
        deviceContinuity: DeviceContinuity,
        credentialVersion: UUID = UUID(),
        installBinding: RefreshInstallBinding? = nil
    ) {
        self.accessToken = accessToken
        self.refreshToken = refreshToken
        self.expiresAt = expiresAt
        self.recoveryEpoch = recoveryEpoch
        self.deviceContinuity = deviceContinuity
        self.credentialVersion = credentialVersion
        self.installBinding = installBinding
    }
}

// MARK: - Store

public protocol SecureSessionStore: Sendable {
    func loadAccount(_ key: AccountSessionKey) throws -> SessionCredentials?
    func saveAccount(_ credentials: SessionCredentials, for key: AccountSessionKey) throws
    func rekeyAccount(_ oldKey: AccountSessionKey, to newKey: AccountSessionKey, checkpointID: UUID) throws
    func loadDeviceContinuity(for key: ContinuityKey) throws -> DeviceContinuityBinding?
    func saveDeviceContinuity(_ binding: DeviceContinuityBinding, for key: ContinuityKey) throws
    func rekeyFamily(_ oldKey: FamilySessionKey, to newKey: FamilySessionKey, checkpointID: UUID) throws
    func promote(_ accountKey: AccountSessionKey, to familyKey: FamilySessionKey, checkpointID: UUID) throws
    func resumePromotion(checkpointID: UUID) throws -> FamilySessionKey?
    func loadFamily(_ key: FamilySessionKey) throws -> SessionCredentials?
    func saveFamily(_ credentials: SessionCredentials, for key: FamilySessionKey) throws
    func deleteFamily(_ key: FamilySessionKey) throws
    func invalidateSessionsRetainingContinuity(for accountKey: AccountSessionKey) throws
    func deleteDeviceContinuity(for key: ContinuityKey) throws
    /// This install's stable identifier, created on first use.
    func installationID() throws -> String
}

public enum SessionStoreError: Error, Sendable, Equatable {
    case missingSource
    case keychain(OSStatus)
}

/// The account/family/continuity item encoding.
///
/// Pinned here rather than derived at each call site: a change to any of these
/// strings silently orphans every stored credential on every existing install,
/// which looks exactly like a mass sign-out.
enum SessionItemEncoding {
    static let service = AppIdentity.identifier("session")

    static func account(_ key: AccountSessionKey) -> String {
        "account|\(base64(key.canonicalOrigin))|\(key.serverInstanceID.uuidString.lowercased())|\(key.accountID.uuidString.lowercased())|\(key.deviceID.uuidString.lowercased())|\(key.recoveryEpoch)"
    }

    static func family(_ key: FamilySessionKey) -> String {
        "family|\(base64(key.canonicalOrigin))|\(key.serverInstanceID.uuidString.lowercased())|\(key.familyID.uuidString.lowercased())|\(key.accountID.uuidString.lowercased())|\(key.deviceID.uuidString.lowercased())|\(key.recoveryEpoch)"
    }

    static func continuity(_ key: ContinuityKey) -> String {
        "continuity|\(base64(key.canonicalOrigin))|\(key.serverInstanceID.uuidString.lowercased())|\(key.accountID.uuidString.lowercased())|\(key.deviceID.uuidString.lowercased())"
    }

    /// Not per account or per server: one install is one installation,
    /// whoever signs in and wherever.
    static let installation = "installation"

    static func promotion(_ checkpointID: UUID) -> String {
        "promotion|\(checkpointID.uuidString.lowercased())"
    }

    private static func base64(_ value: String) -> String { Data(value.utf8).base64EncodedString() }
}

/// The raw item operations a session store needs. Abstracted so tests can run
/// against a simulated backend: the real Keychain is not available to a
/// `swift test` process without an entitled host app.
public protocol KeychainBackend: Sendable {
    func read(service: String, account: String) throws -> Data?
    func write(_ data: Data, service: String, account: String) throws
    func delete(service: String, account: String) throws
}

public final class KeychainSessionStore: SecureSessionStore {
    private let backend: any KeychainBackend

    public init(backend: any KeychainBackend) { self.backend = backend }

    // MARK: Account

    public func loadAccount(_ key: AccountSessionKey) throws -> SessionCredentials? {
        try read(SessionItemEncoding.account(key))
    }

    public func saveAccount(_ credentials: SessionCredentials, for key: AccountSessionKey) throws {
        try write(credentials, to: SessionItemEncoding.account(key))
        try saveDeviceContinuity(
            DeviceContinuityBinding(accountID: key.accountID, continuity: credentials.deviceContinuity),
            for: ContinuityKey(
                canonicalOrigin: key.canonicalOrigin,
                serverInstanceID: key.serverInstanceID,
                accountID: key.accountID,
                deviceID: key.deviceID
            )
        )
    }

    public func rekeyAccount(_ oldKey: AccountSessionKey, to newKey: AccountSessionKey, checkpointID: UUID) throws {
        try replace(
            from: SessionItemEncoding.account(oldKey),
            to: SessionItemEncoding.account(newKey),
            checkpointID: checkpointID,
            resumeValue: nil as FamilySessionKey?
        )
    }

    // MARK: Family

    public func loadFamily(_ key: FamilySessionKey) throws -> SessionCredentials? {
        try read(SessionItemEncoding.family(key))
    }

    public func saveFamily(_ credentials: SessionCredentials, for key: FamilySessionKey) throws {
        try write(credentials, to: SessionItemEncoding.family(key))
    }

    public func deleteFamily(_ key: FamilySessionKey) throws {
        try backend.delete(service: SessionItemEncoding.service, account: SessionItemEncoding.family(key))
    }

    public func rekeyFamily(_ oldKey: FamilySessionKey, to newKey: FamilySessionKey, checkpointID: UUID) throws {
        try replace(
            from: SessionItemEncoding.family(oldKey),
            to: SessionItemEncoding.family(newKey),
            checkpointID: checkpointID,
            resumeValue: newKey
        )
    }

    /// Moves the pre-family session onto its family key in one checkpointed
    /// step, so a crash between the two leaves one item and a resumable
    /// record — never two copies of the same secret.
    ///
    /// Promotion records that membership is ready. It deliberately does not
    /// bind or expose a shared realm: P3.4 owns the single realm activation,
    /// after mapping and canonical-baby establishment.
    public func promote(_ accountKey: AccountSessionKey, to familyKey: FamilySessionKey, checkpointID: UUID) throws {
        try replace(
            from: SessionItemEncoding.account(accountKey),
            to: SessionItemEncoding.family(familyKey),
            checkpointID: checkpointID,
            resumeValue: familyKey
        )
    }

    public func resumePromotion(checkpointID: UUID) throws -> FamilySessionKey? {
        guard let data = try backend.read(
            service: SessionItemEncoding.service,
            account: SessionItemEncoding.promotion(checkpointID)
        ) else { return nil }
        return try JSONDecoder().decode(FamilySessionKey.self, from: data)
    }

    // MARK: Continuity

    public func loadDeviceContinuity(for key: ContinuityKey) throws -> DeviceContinuityBinding? {
        guard let data = try backend.read(
            service: SessionItemEncoding.service,
            account: SessionItemEncoding.continuity(key)
        ) else { return nil }
        return try JSONDecoder().decode(DeviceContinuityBinding.self, from: data)
    }

    public func saveDeviceContinuity(_ binding: DeviceContinuityBinding, for key: ContinuityKey) throws {
        try backend.write(
            try JSONEncoder().encode(binding),
            service: SessionItemEncoding.service,
            account: SessionItemEncoding.continuity(key)
        )
    }

    public func deleteDeviceContinuity(for key: ContinuityKey) throws {
        try backend.delete(service: SessionItemEncoding.service, account: SessionItemEncoding.continuity(key))
    }

    /// Erases every access/refresh secret for exactly this account while
    /// keeping its device-continuity proof.
    ///
    /// The retained item is never returned as `SessionCredentials`, so it
    /// cannot authorize ordinary sync — it can only supply the continuity
    /// binding to a *fresh* Apple exchange. Targeting is exact: another
    /// account's credentials on the same device are untouched.
    public func invalidateSessionsRetainingContinuity(for accountKey: AccountSessionKey) throws {
        try backend.delete(service: SessionItemEncoding.service, account: SessionItemEncoding.account(accountKey))
        for epoch in 0...accountKey.recoveryEpoch {
            // Family items are epoch-qualified; the account's own family
            // sessions are the ones minted at or before its current epoch.
            try deleteFamilyItems(for: accountKey, epoch: epoch)
        }
    }

    private func deleteFamilyItems(for accountKey: AccountSessionKey, epoch: UInt64) throws {
        // The backend enumerates by prefix so an account's family items can be
        // removed without the caller knowing every family ID it ever joined.
        guard let enumerating = backend as? any EnumeratingKeychainBackend else { return }
        let prefix = "family|\(Data(accountKey.canonicalOrigin.utf8).base64EncodedString())|\(accountKey.serverInstanceID.uuidString.lowercased())|"
        let suffix = "|\(accountKey.accountID.uuidString.lowercased())|\(accountKey.deviceID.uuidString.lowercased())|\(epoch)"
        for account in try enumerating.accounts(service: SessionItemEncoding.service)
        where account.hasPrefix(prefix) && account.hasSuffix(suffix) {
            try backend.delete(service: SessionItemEncoding.service, account: account)
        }
    }

    // MARK: Installation

    /// The `installation_id` every Apple exchange carries, created once and
    /// kept.
    ///
    /// In the Keychain, not in defaults: it is what the server binds this
    /// device to, and it must survive exactly as long as the device's
    /// continuity proof does — past an app update, and shared through the App
    /// Group access group with the widget and App Intents. Never deleted by a
    /// sign-out or a disconnect; a new one would only register a new device.
    public func installationID() throws -> String {
        if let data = try backend.read(service: SessionItemEncoding.service, account: SessionItemEncoding.installation),
           let existing = String(data: data, encoding: .utf8), !existing.isEmpty {
            return existing
        }
        let created = UUID().uuidString.lowercased()
        try backend.write(Data(created.utf8), service: SessionItemEncoding.service, account: SessionItemEncoding.installation)
        return created
    }

    // MARK: Plumbing

    private func read(_ account: String) throws -> SessionCredentials? {
        guard let data = try backend.read(service: SessionItemEncoding.service, account: account) else { return nil }
        return try JSONDecoder().decode(SessionCredentials.self, from: data)
    }

    private func write(_ credentials: SessionCredentials, to account: String) throws {
        try backend.write(try JSONEncoder().encode(credentials), service: SessionItemEncoding.service, account: account)
    }

    /// Checkpointed move: record where the secret is going, write it at the
    /// destination, then remove the source. A crash at any point leaves the
    /// checkpoint to resume from and at most one live copy.
    private func replace<Resume: Encodable>(
        from source: String,
        to destination: String,
        checkpointID: UUID,
        resumeValue: Resume?
    ) throws {
        guard let data = try backend.read(service: SessionItemEncoding.service, account: source) else {
            // Already moved: a resumed promotion must be idempotent.
            if try backend.read(service: SessionItemEncoding.service, account: destination) != nil { return }
            throw SessionStoreError.missingSource
        }
        if let resumeValue {
            try backend.write(
                try JSONEncoder().encode(resumeValue),
                service: SessionItemEncoding.service,
                account: SessionItemEncoding.promotion(checkpointID)
            )
        }
        try backend.write(data, service: SessionItemEncoding.service, account: destination)
        try backend.delete(service: SessionItemEncoding.service, account: source)
    }
}

/// Backends that can list their accounts, which the credential-state purge
/// needs to find an account's family items.
public protocol EnumeratingKeychainBackend: KeychainBackend {
    func accounts(service: String) throws -> [String]
}
