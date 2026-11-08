import Foundation
import SwiftData
import BabyCore
import BabyProtocol

public enum ImmunizationStoreError: Error, Equatable, Sendable {
    case babyUnavailable
    case entryUnavailable
    case invalidDose
    case invalidDate
    case invalidTimeZone
    case futureDate
    case beforeBirth
}

public extension BabyStore {
    static let immunizationDoses = 1...10

    /// What makes two vaccine names the same vaccine: the supplement name
    /// key. The one rule the names list, the next dose, the panel's picker
    /// and the Immunizations screen's sections all use.
    nonisolated static func vaccineNameKey(_ name: String) -> String { supplementNameKey(name) }

    @discardableResult
    func logImmunization(vaccine: String, dose: Int?, for babyID: UUID, at date: Date? = nil,
                         note: String? = nil, expectedWriteContext: RealmWriteContext? = nil) throws -> ImmunizationEntry {
        let expected = try expectedWriteContext ?? captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let when = date ?? self.now
            let zone = TimeZone.current.identifier
            try self.validateImmunization(dose: dose, for: babyID, at: when, timeZoneID: zone)
            let name = try self.savedVaccineName(vaccine)
            let entry = ImmunizationEntry(babyID: babyID, vaccine: name, dose: dose, givenAt: when, timeZoneID: zone,
                                          modifiedAt: self.now, createdBy: self.deviceID, realm: self.activeRealm)
            entry.note = note
            self.context.insert(entry)
            try self.recordLocked(.create, on: entry)
            return entry
        }
    }

    func editImmunization(_ entry: ImmunizationEntry, vaccine: String, dose: Int?, at date: Date, note: String?,
                          expectedWriteContext: RealmWriteContext? = nil) throws {
        let expected = try expectedWriteContext ?? captureWriteContext()
        try withRealmWrite(expected: expected) {
            guard entry.realmKey == expected.realm.encodedKey else { throw RealmWriteError.wrongRealm }
            guard entry.modelContext === self.context, !entry.isTombstoned else { throw ImmunizationStoreError.entryUnavailable }
            try self.validateImmunization(dose: dose, for: entry.babyID, at: date, timeZoneID: entry.timeZoneID,
                                          unchangedFrom: entry.givenAt)
            entry.vaccine = try self.savedVaccineName(vaccine, excluding: entry.id)
            entry.dose = dose
            entry.givenAt = date
            entry.note = note
            entry.lastModifiedAt = self.now
            try self.recordLocked(.update, on: entry)
        }
    }

    /// Newest shot first.
    func immunizations(for babyID: UUID) throws -> [ImmunizationEntry] {
        try immunizationRows().filter { $0.babyID == babyID }
    }

    func lastImmunization(for babyID: UUID) throws -> ImmunizationEntry? {
        try immunizations(for: babyID).first
    }

    /// Every vaccine the family has logged for a baby it still has: the one
    /// given most recently first, each in the spelling of its earliest shot.
    /// A deleted baby's shots stay in the store until the server's cascade
    /// arrives; their names are not offered to its siblings meanwhile.
    func vaccineNames() throws -> [String] {
        let living = Set(try babies().map(\.id))
        let rows = try immunizationRows().filter { living.contains($0.babyID) }
        var spelling: [String: String] = [:]
        for row in rows.reversed() where spelling[Self.vaccineNameKey(row.vaccine)] == nil {
            spelling[Self.vaccineNameKey(row.vaccine)] = row.vaccine
        }
        var seen: Set<String> = []
        return rows.compactMap { row in
            let key = Self.vaccineNameKey(row.vaccine)
            return seen.insert(key).inserted ? spelling[key] : nil
        }
    }

    /// One more than this baby's highest dose of `vaccine`, at most 10; 1 when
    /// there is none, or the name is not a name yet.
    func nextDose(of vaccine: String, for babyID: UUID) throws -> Int {
        guard let name = try? SupplementName.normalized(vaccine) else { return 1 }
        let key = Self.vaccineNameKey(name)
        let highest = try immunizations(for: babyID)
            .filter { Self.vaccineNameKey($0.vaccine) == key }
            .compactMap(\.dose).max() ?? 0
        return min(highest + 1, Self.immunizationDoses.upperBound)
    }
}

extension BabyStore {
    /// The active realm's live shots, newest `givenAt` first. Fetched ascending
    /// and reversed, so of two shots at one instant the later insert is first
    /// (see `lastFeed`).
    func immunizationRows() throws -> [ImmunizationEntry] {
        let key = activeRealm.encodedKey
        var descriptor = FetchDescriptor<ImmunizationEntry>(predicate: #Predicate {
            $0.realmKey == key && !$0.isTombstoned
        })
        descriptor.sortBy = [SortDescriptor(\.givenAt, order: .forward)]
        return Array(try context.fetch(descriptor).reversed())
    }

    /// `raw` normalised, in the spelling the family already uses for it.
    func savedVaccineName(_ raw: String, excluding id: UUID? = nil) throws -> String {
        let name = try SupplementName.normalized(raw)
        let key = Self.vaccineNameKey(name)
        return try immunizationRows().reversed().first {
            $0.id != id && Self.vaccineNameKey($0.vaccine) == key
        }?.vaccine ?? name
    }

    /// Growth's date rule (`validateMeasurement`): not after now, not before
    /// the birth day in the entry's own zone. An edit that leaves the time
    /// where it was is never refused as future: a shot the other phone logged
    /// with its clock running fast can still have its dose or note corrected.
    func validateImmunization(dose: Int?, for babyID: UUID, at date: Date, timeZoneID: String,
                              unchangedFrom existing: Date? = nil) throws {
        if let dose, !Self.immunizationDoses.contains(dose) { throw ImmunizationStoreError.invalidDose }
        guard let baby = try baby(babyID), !baby.isTombstoned else { throw ImmunizationStoreError.babyUnavailable }
        guard date.timeIntervalSince1970.isFinite, baby.birthDate.timeIntervalSince1970.isFinite else {
            throw ImmunizationStoreError.invalidDate
        }
        guard let zone = TimeZone(identifier: timeZoneID) else { throw ImmunizationStoreError.invalidTimeZone }
        guard date <= now || date == existing else { throw ImmunizationStoreError.futureDate }
        let givenDay = try LocalDate(displaying: date, in: zone)
        let birthDay = try baby.localBirthDate ?? LocalDate(displaying: baby.birthDate, in: .current)
        guard givenDay >= birthDay else { throw ImmunizationStoreError.beforeBirth }
    }
}
