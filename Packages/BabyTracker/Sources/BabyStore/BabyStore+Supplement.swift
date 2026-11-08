import Foundation
import SwiftData
import BabyProtocol

public extension BabyStore {
    @discardableResult
    func logSupplement(name: String, for babyID: UUID, at date: Date? = nil,
                       note: String? = nil, expectedWriteContext: RealmWriteContext? = nil) throws -> SupplementEntry {
        let expected = try expectedWriteContext ?? captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let name = try self.savedSupplementName(name, for: babyID)
            let entry = SupplementEntry(babyID: babyID, name: name, startedAt: date ?? self.now,
                timeZoneID: TimeZone.current.identifier, loggedAt: self.now, createdBy: self.deviceID, realm: self.activeRealm)
            entry.note = note
            self.context.insert(entry)
            try self.recordLocked(.create, on: entry)
            return entry
        }
    }

    func supplementNames(for babyID: UUID) throws -> [String] {
        let rows = try supplementRows(for: babyID)
        // Select spelling from the first surviving log, but position from the
        // most recent use. Remote caregivers may have used different casing.
        var spelling: [String: String] = [:]
        for row in rows.reversed() where spelling[Self.supplementNameKey(row.name)] == nil {
            spelling[Self.supplementNameKey(row.name)] = row.name
        }
        var seen: Set<String> = []
        return rows.compactMap { row in
            let key = Self.supplementNameKey(row.name)
            return seen.insert(key).inserted ? spelling[key] : nil
        }
    }

    func lastSupplement(for babyID: UUID) throws -> SupplementEntry? {
        try supplementRows(for: babyID).first
    }

    func editSupplement(_ entry: SupplementEntry, name: String, at date: Date, note: String?,
                        expectedWriteContext: RealmWriteContext? = nil) throws {
        let expected = try expectedWriteContext ?? captureWriteContext()
        try withRealmWrite(expected: expected) {
            guard entry.realmKey == expected.realm.encodedKey else { throw RealmWriteError.wrongRealm }
            entry.name = try self.savedSupplementName(name, for: entry.babyID)
            entry.startedAt = date
            entry.note = note
            entry.lastModifiedAt = self.now
            try self.recordLocked(.update, on: entry)
        }
    }
}

extension BabyStore {
    /// Most recently logged first. Fetched ascending and read from the end
    /// rather than fetched descending and read from the front — the same
    /// reasoning as `LogDefaults.lastBottleContents`: SwiftData's sort is
    /// stable, and two rows that tie on `loggedAt` (two different names
    /// logged in the same instant) keep their original insertion order
    /// regardless of the requested sort direction. The previous tie-break —
    /// comparing `id.uuidString`, a random value — made that case a coin
    /// flip; reading the tail of an ascending fetch makes it deterministic.
    func supplementRows(for babyID: UUID) throws -> [SupplementEntry] {
        let key = activeRealm.encodedKey
        var descriptor = FetchDescriptor<SupplementEntry>(predicate: #Predicate {
            $0.realmKey == key && $0.babyID == babyID && !$0.isTombstoned
        })
        descriptor.sortBy = [SortDescriptor(\.loggedAt, order: .forward)]
        return Array(try context.fetch(descriptor).reversed())
    }

    nonisolated static func supplementNameKey(_ name: String) -> String {
        name.folding(options: [.caseInsensitive], locale: Locale(identifier: "en_US_POSIX"))
    }

    func savedSupplementName(_ raw: String, for babyID: UUID, excluding id: UUID? = nil) throws -> String {
        let name = try SupplementName.normalized(raw)
        let key = Self.supplementNameKey(name)
        return try supplementRows(for: babyID).reversed().first {
            $0.id != id && Self.supplementNameKey($0.name) == key
        }?.name ?? name
    }
}
