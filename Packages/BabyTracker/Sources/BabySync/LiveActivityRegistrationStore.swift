import BabyProtocol
import BabyStore
import Foundation

/// Who a registration ledger belongs to: one account on one device on one
/// server instance and recovery epoch. Not the family — a family switch keeps
/// the installation, because the server fences the old family by a newer
/// installation version.
public struct LiveActivityRegistrationIdentity: Codable, Equatable, Sendable {
    public let canonicalOrigin: String
    public let serverInstanceID: UUID
    public let recoveryEpoch: UInt64
    public let accountID: UUID
    public let deviceID: UUID

    public init(familyKey: FamilySessionKey) {
        canonicalOrigin = familyKey.canonicalOrigin
        serverInstanceID = familyKey.serverInstanceID
        recoveryEpoch = familyKey.recoveryEpoch
        accountID = familyKey.accountID
        deviceID = familyKey.deviceID
    }
}

/// The non-secret registration state, shared by every process in the App
/// Group. Versions here only ever increase. No token is ever written to it.
public struct LiveActivityRegistrationLedger: Codable, Equatable, Sendable {
    public static let currentSchema = 1

    public var schema = Self.currentSchema
    public var identity: LiveActivityRegistrationIdentity?
    /// An opaque logical registration identity, not a hardware identifier.
    /// Rotated whenever the identity changes or the ledger cannot be trusted.
    public var installationID: UUID?
    /// The highest installation registration version ever allocated: device
    /// registrations and removals share it, across families.
    public var registrationVersion: Int64 = 0
    /// The last device registration sent, and whether it was accepted.
    public var device: Device?
    public var activities: [String: Activity] = [:]
    /// Removals not yet confirmed, each scoped to the family whose session
    /// must send it.
    public var pendingRemovals: [PendingRemoval] = []
    /// The highest foreground inventory version ever allocated.
    public var inventoryVersion: Int64 = 0
    /// A foreground absence proof not yet acknowledged, retried identically.
    public var pendingInventory: LiveActivityForegroundInventory?
    /// The family whose receipt the pending proof answers; it goes nowhere else.
    public var pendingInventoryFamilyID: UUID?
    /// From the latest accepted device receipt.
    public var unconfirmedStarts: [LiveActivityUnconfirmedStart] = []
    /// Duplicate cards the card controller ended: never registered again,
    /// above all not as ended, which the server reads as a dismissal of the
    /// lifecycle the surviving card shares. Optional so a ledger written
    /// before it decodes unchanged.
    public var retiredActivityIDs: [String]?

    public init() {}

    public struct Device: Codable, Equatable, Sendable {
        public var familyID: UUID
        public var version: Int64
        public var environment: LiveActivityAPNsEnvironment
        public var feedBreakMinutes: Int
        public var accepted: Bool
        /// Taken down: a later registration needs a newer version.
        public var revoked: Bool = false
        /// The server refused this version as not newer: never resend it.
        public var superseded: Bool = false
    }

    public struct Activity: Codable, Equatable, Sendable {
        public var scope: LiveActivityScope
        /// The highest token version ever allocated for this activity.
        public var tokenVersion: Int64
        public var sent: Sent?
        public var accepted: Bool
        /// The server's authoritative generation for this card.
        public var canonicalGenerationID: UUID?
        public var superseded: Bool = false

        public struct Sent: Codable, Equatable, Sendable {
            public var version: Int64
            public var state: LiveActivityRegistrationState
            public var knownGenerationID: UUID?
            public var timerID: UUID?
            public var sessionID: UUID?
            public var environment: LiveActivityAPNsEnvironment
        }
    }

    public struct PendingRemoval: Codable, Equatable, Sendable {
        public var familyID: UUID
        public var installationID: UUID
        public var version: Int64
        public init(familyID: UUID, installationID: UUID, version: Int64) {
            self.familyID = familyID
            self.installationID = installationID
            self.version = version
        }
    }
}

/// The ledger file, its cross-process lock, and the Keychain slots that hold
/// the tokens it must never contain.
///
/// Every read-modify-write runs under `AppGroupBootstrapLock` (flock on a
/// separate file), so the app and an App Intent process cannot both allocate
/// the same version or lose each other's removals. Nothing awaits or touches
/// the network while the lock is held. Files use until-first-unlock
/// protection so a locked phone's background launch can still read them.
public final class LiveActivityRegistrationStore: @unchecked Sendable {
    public enum TokenSlot: Sendable, Equatable {
        case pushToStart(UUID)
        case activity(UUID, String)

        var account: String {
            switch self {
            case .pushToStart(let installation): "start|\(installation.uuidString.lowercased())"
            case .activity(let installation, let activityID): "activity|\(installation.uuidString.lowercased())|\(activityID)"
            }
        }

        var installationID: UUID {
            switch self {
            case .pushToStart(let installation), .activity(let installation, _): installation
            }
        }
    }

    public enum StoreError: Error, Equatable, Sendable { case unreadable }

    static let keychainService = "com.olhapi.babytracker.liveactivity"
    static let bindingAccount = "installation-binding"

    public let directory: URL
    public var ledgerURL: URL { directory.appending(path: "registration-ledger.json") }
    var lockURL: URL { directory.appending(path: "registration-ledger.lock") }
    private let keychain: any KeychainBackend
    private let lockTimeout: TimeInterval

    public init(directory: URL, keychain: any KeychainBackend, lockTimeout: TimeInterval = 5) {
        self.directory = directory
        self.keychain = keychain
        self.lockTimeout = lockTimeout
    }

    /// The shared App Group location, next to the store the widget reads.
    public static func appGroup(keychain: any KeychainBackend) -> LiveActivityRegistrationStore {
        LiveActivityRegistrationStore(
            directory: BabySchema.defaultGroupDirectory.appending(path: "Library/Application Support/LiveActivityPush", directoryHint: .isDirectory),
            keychain: keychain
        )
    }

    // MARK: Ledger

    public func read() throws -> LiveActivityRegistrationLedger {
        try locked { try load() }
    }

    /// Atomic read-modify-write under the cross-process lock. Writes only when
    /// the closure changed something.
    @discardableResult
    public func update<T>(_ body: (inout LiveActivityRegistrationLedger) throws -> T) throws -> T {
        try locked {
            let before = try load()
            var ledger = before
            let result = try body(&ledger)
            if ledger != before { try save(ledger) }
            return result
        }
    }

    /// Returns this identity's installation, rotating it — and discarding
    /// every scoped version, removal, mapping and token — when the stored
    /// ledger belongs to another account, device, server or epoch, cannot be
    /// read, or lacks its this-device-only Keychain binding (a ledger restored
    /// from a backup onto another phone, or reinstalled over a stale one).
    @discardableResult
    public func bind(_ identity: LiveActivityRegistrationIdentity) throws -> UUID {
        try locked {
            let existing = try? load()
            let binding = try keychain.read(service: Self.keychainService, account: Self.bindingAccount)
                .flatMap { UUID(uuidString: String(decoding: $0, as: UTF8.self)) }
            if let existing, existing.identity == identity, let installation = existing.installationID, binding == installation {
                return installation
            }
            // Whatever installation the old ledger or binding named, its
            // tokens go with it.
            for old in Set([existing?.installationID, binding].compactMap { $0 }) {
                deleteTokens(of: old, ledger: existing)
            }
            var fresh = LiveActivityRegistrationLedger()
            fresh.identity = identity
            let installation = UUID()
            fresh.installationID = installation
            try save(fresh)
            try keychain.write(Data(installation.uuidString.utf8), service: Self.keychainService, account: Self.bindingAccount)
            return installation
        }
    }

    // MARK: Tokens (Keychain only)

    public func saveToken(_ token: String, for slot: TokenSlot) throws {
        try keychain.write(Data(token.utf8), service: Self.keychainService, account: slot.account)
    }

    public func token(for slot: TokenSlot) throws -> String? {
        try keychain.read(service: Self.keychainService, account: slot.account).map { String(decoding: $0, as: UTF8.self) }
    }

    public func deleteToken(for slot: TokenSlot) throws {
        try keychain.delete(service: Self.keychainService, account: slot.account)
    }

    private func deleteTokens(of installation: UUID, ledger: LiveActivityRegistrationLedger?) {
        try? deleteToken(for: .pushToStart(installation))
        for activityID in ledger?.activities.keys.map({ $0 }) ?? [] {
            try? deleteToken(for: .activity(installation, activityID))
        }
        // An unreadable ledger cannot list its activities; the Keychain can.
        guard let enumerating = keychain as? any EnumeratingKeychainBackend,
              let accounts = try? enumerating.accounts(service: Self.keychainService) else { return }
        let prefix = "activity|\(installation.uuidString.lowercased())|"
        for account in accounts where account.hasPrefix(prefix) {
            try? keychain.delete(service: Self.keychainService, account: account)
        }
    }

    // MARK: Files

    private func locked<T>(_ body: () throws -> T) throws -> T {
        try prepareDirectory()
        let creatingLock = !FileManager.default.fileExists(atPath: lockURL.path)
        return try AppGroupBootstrapLock.withLock(at: lockURL, timeout: lockTimeout) {
            if creatingLock { protect(lockURL) }
            return try body()
        }
    }

    private func load() throws -> LiveActivityRegistrationLedger {
        guard FileManager.default.fileExists(atPath: ledgerURL.path) else { return LiveActivityRegistrationLedger() }
        let data = try Data(contentsOf: ledgerURL)
        guard let ledger = try? JSONDecoder().decode(LiveActivityRegistrationLedger.self, from: data),
              ledger.schema == LiveActivityRegistrationLedger.currentSchema else { throw StoreError.unreadable }
        return ledger
    }

    private func save(_ ledger: LiveActivityRegistrationLedger) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let data = try encoder.encode(ledger)
        #if os(iOS)
        try data.write(to: ledgerURL, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
        #else
        try data.write(to: ledgerURL, options: [.atomic])
        #endif
    }

    private func prepareDirectory() throws {
        guard !FileManager.default.fileExists(atPath: directory.path) else { return }
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        protect(directory)
    }

    /// Kept out of backups (the directory's flag covers the atomically
    /// replaced ledger; a restored ledger is rejected anyway) and readable
    /// after first unlock, which a locked phone's background launch needs.
    private func protect(_ url: URL) {
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var target = url
        try? target.setResourceValues(values)
        #if os(iOS)
        try? FileManager.default.setAttributes(
            [.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication], ofItemAtPath: url.path
        )
        #endif
    }
}
