import Foundation
import Security

/// The real Keychain, scoped to the App Group access group so the app, the
/// widget extension and App Intents all see one set of items.
public struct SystemKeychainBackend: EnumeratingKeychainBackend {
    public let accessGroup: String?

    public init(accessGroup: String? = nil) { self.accessGroup = accessGroup }

    private func baseQuery(service: String, account: String?) -> [String: Any] {
        var query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
        ]
        if let account { query[kSecAttrAccount as String] = account }
        if let accessGroup { query[kSecAttrAccessGroup as String] = accessGroup }
        return query
    }

    public func read(service: String, account: String) throws -> Data? {
        var query = baseQuery(service: service, account: account)
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess else { throw SessionStoreError.keychain(status) }
        return item as? Data
    }

    public func write(_ data: Data, service: String, account: String) throws {
        let query = baseQuery(service: service, account: account)
        let attributes: [String: Any] = [
            kSecValueData as String: data,
            // Secrets must not leave the device in a backup, and must be
            // unavailable before first unlock — a widget refresh on a locked
            // phone has no business reading a refresh token.
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        let updateStatus = SecItemUpdate(query as CFDictionary, attributes as CFDictionary)
        if updateStatus == errSecSuccess { return }
        if updateStatus == errSecItemNotFound {
            var insert = query
            insert.merge(attributes) { _, new in new }
            let addStatus = SecItemAdd(insert as CFDictionary, nil)
            guard addStatus == errSecSuccess else { throw SessionStoreError.keychain(addStatus) }
            return
        }
        throw SessionStoreError.keychain(updateStatus)
    }

    public func delete(service: String, account: String) throws {
        let status = SecItemDelete(baseQuery(service: service, account: account) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw SessionStoreError.keychain(status)
        }
    }

    public func accounts(service: String) throws -> [String] {
        var query = baseQuery(service: service, account: nil)
        query[kSecReturnAttributes as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitAll
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecItemNotFound { return [] }
        guard status == errSecSuccess else { throw SessionStoreError.keychain(status) }
        return ((item as? [[String: Any]]) ?? []).compactMap { $0[kSecAttrAccount as String] as? String }
    }
}

/// A simulated Keychain for tests and previews.
///
/// Real `SecItem` calls need an entitled host application, which a `swift test`
/// process is not, so credential-isolation behaviour is proven against this
/// instead. It stores items under the same `(service, account)` encoding the
/// system backend uses, so the isolation it demonstrates is the encoding's,
/// not the fake's.
public final class InMemoryKeychainBackend: EnumeratingKeychainBackend, @unchecked Sendable {
    private let lock = NSLock()
    private var items: [String: Data] = [:]

    public init() {}

    private func slot(_ service: String, _ account: String) -> String { "\(service)\u{0}\(account)" }

    public func read(service: String, account: String) throws -> Data? {
        lock.lock(); defer { lock.unlock() }
        return items[slot(service, account)]
    }

    public func write(_ data: Data, service: String, account: String) throws {
        lock.lock(); defer { lock.unlock() }
        items[slot(service, account)] = data
    }

    public func delete(service: String, account: String) throws {
        lock.lock(); defer { lock.unlock() }
        items[slot(service, account)] = nil
    }

    public func accounts(service: String) throws -> [String] {
        lock.lock(); defer { lock.unlock() }
        return items.keys.compactMap { key in
            let parts = key.split(separator: "\u{0}", maxSplits: 1, omittingEmptySubsequences: false)
            guard parts.count == 2, String(parts[0]) == service else { return nil }
            return String(parts[1])
        }.sorted()
    }

    /// Survives a simulated relaunch: the same backing store, a new store object.
    public func snapshot() -> InMemoryKeychainBackend {
        lock.lock(); defer { lock.unlock() }
        let copy = InMemoryKeychainBackend()
        copy.items = items
        return copy
    }
}
