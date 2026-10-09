import Foundation
import SwiftData
import BabyCore
import BabyProtocol

public extension BabyStore {
    func addBaby(name: String, birthDate: Date, referenceSex: GrowthReferenceSex, expectedDueDate: LocalDate?, expectedWriteContext: RealmWriteContext? = nil) throws -> Baby {
        let expected = try expectedWriteContext ?? captureWriteContext()
        return try withRealmWrite(expected: expected) {
            guard !name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { throw BabyEditError.emptyName }
            guard birthDate.timeIntervalSince1970.isFinite else { throw GrowthStoreError.invalidDate }
            _ = try LocalDate(displaying: birthDate, in: .current)
            let existing = try self.babies()
            let baby = Baby(name: name, birthDate: birthDate, sortOrder: existing.count, realm: self.activeRealm)
            baby.iconID = BabyAvatar.assign(sex: referenceSex, siblings: existing.map(\.iconID))
            self.context.insert(baby)
            try self.recordLocked(.create, on: baby)
            try self.setGrowthProfileLocked(for: baby.id, referenceSex: referenceSex, expectedDueDate: expectedDueDate)
            return baby
        }
    }

    /// Gives every living baby that has Boys/Girls an icon from its set.
    ///
    /// A baby with no icon, or one from the other set, first takes the icon
    /// in its server copy (`CanonicalEntityBase`) when that one fits. That
    /// is server state already, so it is written without a change or an
    /// outbox row — an older build kept the other phone's icon only there,
    /// and recording a full update from a stale row would revert newer
    /// server edits such as a rename.
    ///
    /// Only then are the rest assigned and recorded, in list order then id,
    /// each pick counting the ones before it, so two phones doing this at
    /// once write the same ids. In a family that waits for
    /// `sharedRealmSynced` — a sync that succeeded since launch — so the row
    /// being sent is not older than the server's. The local realm ignores
    /// it. Returns how many babies changed; writes nothing when nothing is
    /// missing. Assigning in a read-only realm throws from the write boundary.
    @discardableResult
    func assignMissingBabyIcons(sharedRealmSynced: Bool) throws -> Int {
        let missing = try babies().compactMap { baby -> (Baby, GrowthReferenceSex)? in
            guard let sex = try growthProfile(for: baby.id)?.referenceSex,
                  !Self.iconFits(baby.iconID, sex: sex) else { return nil }
            return (baby, sex)
        }
        guard !missing.isEmpty else { return 0 }

        var adopted = 0
        let adoptable = try missing.compactMap { baby, sex in
            try baseIconID(for: baby.id).flatMap { Self.iconFits($0, sex: sex) ? (baby.id, $0) : nil }
        }
        if !adoptable.isEmpty {
            let expected = try captureWriteContext()
            adopted = try withRealmWrite(expected: expected, allowsReadOnly: true) {
                var count = 0
                for (id, icon) in adoptable {
                    guard let baby = try self.baby(id), !baby.isTombstoned,
                          let sex = try self.growthProfile(for: id)?.referenceSex,
                          !Self.iconFits(baby.iconID, sex: sex) else { continue }
                    baby.iconID = icon
                    count += 1
                }
                return count
            }
        }

        guard !realmIsShared || sharedRealmSynced, missing.count > adopted else { return adopted }
        let expected = try captureWriteContext()
        return try adopted + withRealmWrite(expected: expected) {
            let all = try self.babies().sorted { ($0.sortOrder, $0.id.uuidString) < ($1.sortOrder, $1.id.uuidString) }
            var assigned = 0
            for baby in all {
                guard let sex = try self.growthProfile(for: baby.id)?.referenceSex,
                      !Self.iconFits(baby.iconID, sex: sex) else { continue }
                baby.iconID = BabyAvatar.assign(sex: sex, siblings: all.filter { $0.id != baby.id }.map(\.iconID))
                baby.lastModifiedAt = self.now
                try self.recordLocked(.update, on: baby)
                assigned += 1
            }
            return assigned
        }
    }

    /// The icon in the last server-canonical copy of this baby, if any.
    private func baseIconID(for babyID: UUID) throws -> String? {
        let storageKey = "\(activeRealm.encodedKey)|\(WireEntityKind.baby.rawValue)|\(babyID.uuidString)"
        guard let base = try context.fetch(
            FetchDescriptor<CanonicalEntityBase>(predicate: #Predicate { $0.storageKey == storageKey })
        ).first else { return nil }
        let fields = (try? JSONSerialization.jsonObject(with: base.payload)) as? [String: Any]
        return fields?["icon"] as? String
    }

    /// Whether `iconID` is one of the icons of `sex`'s set.
    internal static func iconFits(_ iconID: String?, sex: GrowthReferenceSex) -> Bool {
        iconID.map(BabyAvatar.ids(for: sex).contains) ?? false
    }

    func babies(in realm: RealmID? = nil) throws -> [Baby] {
        let realmKey = (realm ?? activeRealm).encodedKey
        var descriptor = FetchDescriptor<Baby>(predicate: #Predicate { $0.realmKey == realmKey && !$0.isTombstoned })
        descriptor.sortBy = [SortDescriptor(\.sortOrder, order: .forward)]
        return try context.fetch(descriptor)
    }

    func baby(_ id: UUID, in realm: RealmID? = nil) throws -> Baby? {
        let realmKey = (realm ?? activeRealm).encodedKey
        return try context.fetch(FetchDescriptor<Baby>(predicate: #Predicate { $0.id == id && $0.realmKey == realmKey })).first
    }

    func pendingOutbox(in realm: RealmID? = nil) throws -> [SyncOutboxEntry] {
        let realmKey = (realm ?? activeRealm).encodedKey
        return try context.fetch(FetchDescriptor<SyncOutboxEntry>(predicate: #Predicate { $0.realmKey == realmKey }))
    }

    func legacyChangeLog(in realm: RealmID? = nil) throws -> [ChangeLogEntry] {
        let realmKey = (realm ?? activeRealm).encodedKey
        var descriptor = FetchDescriptor<ChangeLogEntry>(predicate: #Predicate { $0.realmKey == realmKey })
        descriptor.sortBy = [SortDescriptor(\.timestamp)]
        return try context.fetch(descriptor)
    }

    func importSummary() throws -> LegacyImportSummary {
        let key = "legacy-migration-summary"
        guard let row = try context.fetch(FetchDescriptor<ImportCheckpoint>(predicate: #Predicate { $0.key == key })).first else {
            return LegacyImportSummary(convertedBirthDates: [])
        }
        return try JSONDecoder().decode(LegacyImportSummary.self, from: row.payload)
    }

    func migratedEntityKinds(in realm: RealmID? = nil) throws -> [MigratedEntityKind] {
        let key = (realm ?? activeRealm).encodedKey
        var kinds: [MigratedEntityKind] = []
        if !(try context.fetch(FetchDescriptor<Baby>(predicate: #Predicate { $0.realmKey == key }))).isEmpty { kinds.append(.baby) }
        if !(try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate { $0.realmKey == key }))).isEmpty { kinds.append(.feed) }
        if !(try context.fetch(FetchDescriptor<SleepEntry>(predicate: #Predicate { $0.realmKey == key }))).isEmpty { kinds.append(.sleep) }
        if !(try context.fetch(FetchDescriptor<DiaperEntry>(predicate: #Predicate { $0.realmKey == key }))).isEmpty { kinds.append(.diaper) }
        if !(try context.fetch(FetchDescriptor<SupplementEntry>(predicate: #Predicate { $0.realmKey == key }))).isEmpty { kinds.append(.supplement) }
        if !(try context.fetch(FetchDescriptor<GrowthProfile>(predicate: #Predicate { $0.realmKey == key }))).isEmpty { kinds.append(.growthProfile) }
        if !(try context.fetch(FetchDescriptor<MeasurementCheckIn>(predicate: #Predicate { $0.realmKey == key }))).isEmpty { kinds.append(.measurement) }
        if !(try context.fetch(FetchDescriptor<ActiveTimer>(predicate: #Predicate { $0.realmKey == key }))).isEmpty { kinds.append(.activeTimer) }
        if !(try context.fetch(FetchDescriptor<ChangeLogEntry>(predicate: #Predicate { $0.realmKey == key }))).isEmpty { kinds.append(.changeLog) }
        return kinds
    }

    enum BabyEditError: Error {
        case emptyName
    }

    /// A name and a birth date are corrected together because they are the
    /// only two things a baby has, and the sheet that edits one edits both.
    func renameBaby(_ baby: Baby, to name: String, birthDate: Date) throws {
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { throw BabyEditError.emptyName }

        baby.name = trimmed
        baby.birthDate = birthDate
        try update(baby)
    }

    /// Tombstone. Entries belonging to this baby are deliberately left intact —
    /// hiding a child must not destroy their log.
    func deleteBaby(_ baby: Baby) throws {
        try softDelete(baby)
    }

    /// Re-stamps lastModifiedAt and logs the update. Callers mutate the object
    /// first, then call this.
    func update(_ entry: some ChangeLoggable & Touchable) throws {
        entry.lastModifiedAt = now
        try record(.update, on: entry)
    }

    /// Tombstone. The row is never removed.
    func softDelete(_ entry: some ChangeLoggable & Touchable) throws {
        entry.isTombstoned = true
        entry.lastModifiedAt = now
        try record(.delete, on: entry)
    }
}

/// Anything carrying the common mutable bookkeeping columns.
public protocol Touchable: AnyObject {
    var lastModifiedAt: Date { get set }
    var isTombstoned: Bool { get set }
}

extension Baby: Touchable {}
extension FeedEntry: Touchable {}
extension SleepEntry: Touchable {}
extension DiaperEntry: Touchable {}
