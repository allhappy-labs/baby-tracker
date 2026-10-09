import Foundation
import SwiftData
import BabyCore

public enum GrowthStoreError: Error, Equatable, Sendable {
    case babyUnavailable
    case measurementUnavailable
    case invalidDate
    case invalidTimeZone
    case futureMeasurement
    case measurementBeforeBirth
}

public extension BabyStore {
    func growthProfile(for babyID: UUID) throws -> GrowthProfile? {
        let key = activeRealm.encodedKey
        return try context.fetch(FetchDescriptor<GrowthProfile>(predicate: #Predicate {
            $0.realmKey == key && $0.babyID == babyID && !$0.isTombstoned
        })).first
    }

    func setGrowthProfile(for babyID: UUID, referenceSex: GrowthReferenceSex, expectedDueDate: LocalDate?) throws {
        let expected = try captureWriteContext()
        try withGrowthWrite(expected: expected) {
            _ = try self.growthBaby(babyID)
            try self.setGrowthProfileLocked(for: babyID, referenceSex: referenceSex, expectedDueDate: expectedDueDate)
        }
    }

    /// A legacy baby may still have no profile. A nil reference leaves its
    /// existing profile state untouched; it never guesses a reference.
    func updateBabyAndGrowthProfile(_ baby: Baby, name: String, birthDate: Date,
                                   referenceSex: GrowthReferenceSex?, expectedDueDate: LocalDate?) throws {
        let expected = try captureWriteContext()
        try withGrowthWrite(expected: expected, restoring: baby) {
            guard baby.realmKey == expected.realm.encodedKey else { throw RealmWriteError.wrongRealm }
            guard baby.modelContext === self.context else { throw GrowthStoreError.babyUnavailable }
            _ = try self.growthBaby(baby.id)
            let name = name.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !name.isEmpty else { throw BabyEditError.emptyName }
            guard birthDate.timeIntervalSince1970.isFinite else { throw GrowthStoreError.invalidDate }
            let birthDay = try LocalDate(displaying: birthDate, in: .current)
            baby.name = name
            baby.birthDate = birthDate
            baby.birthDateISO = birthDay.iso8601
            // Compared against the profile's old sex: setGrowthProfileLocked
            // has not run yet. An icon from the other set is replaced too.
            // Set before recording so the update carries it.
            if let referenceSex, try !Self.iconFits(baby.iconID, sex: referenceSex) || self.growthProfile(for: baby.id)?.referenceSex != referenceSex {
                let siblings = try self.babies().filter { $0.id != baby.id }.map(\.iconID)
                baby.iconID = BabyAvatar.assign(sex: referenceSex, siblings: siblings)
            }
            baby.lastModifiedAt = self.now
            try self.recordLocked(.update, on: baby)
            if let referenceSex {
                try self.setGrowthProfileLocked(for: baby.id, referenceSex: referenceSex, expectedDueDate: expectedDueDate)
            }
        }
    }

    @discardableResult
    func logMeasurement(for babyID: UUID, at date: Date, timeZoneID: String,
                        values: MeasurementValues) throws -> MeasurementCheckIn {
        let expected = try captureWriteContext()
        return try withGrowthWrite(expected: expected) {
            try self.validateMeasurement(for: babyID, at: date, timeZoneID: timeZoneID)
            let row = MeasurementCheckIn(babyID: babyID, measuredAt: date, timeZoneID: timeZoneID,
                loggedAt: self.now, values: values, createdBy: self.deviceID, realm: expected.realm)
            self.context.insert(row)
            try self.recordLocked(.create, on: row)
            return row
        }
    }

    func editMeasurement(_ entry: MeasurementCheckIn, at date: Date, timeZoneID: String,
                         values: MeasurementValues) throws {
        let expected = try captureWriteContext()
        try withGrowthWrite(expected: expected) {
            try self.requireEditableMeasurement(entry, in: expected.realm)
            try self.validateMeasurement(for: entry.babyID, at: date, timeZoneID: timeZoneID)
            entry.measuredAt = date
            entry.timeZoneID = timeZoneID
            entry.weightKG = values.weightKG
            entry.lengthCM = values.lengthCM
            entry.headCircumferenceCM = values.headCircumferenceCM
            entry.lengthPositionRaw = values.lengthPosition?.rawValue
            entry.lastModifiedAt = self.now
            try self.recordLocked(.update, on: entry)
        }
    }

    /// Most recent measurement first; logging or editing a backdated entry
    /// cannot reorder readings with the same measured time.
    func measurements(for babyID: UUID) throws -> [MeasurementCheckIn] {
        let key = activeRealm.encodedKey
        return try context.fetch(FetchDescriptor<MeasurementCheckIn>(predicate: #Predicate {
            $0.realmKey == key && $0.babyID == babyID && !$0.isTombstoned
        })).sorted {
            $0.measuredAt == $1.measuredAt ? $0.id.uuidString < $1.id.uuidString : $0.measuredAt > $1.measuredAt
        }
    }

    /// Mutates only after permission and realm checks, so a refused deletion
    /// cannot leave an unsaved tombstone visible to the UI.
    func softDelete(_ entry: MeasurementCheckIn) throws {
        let expected = try captureWriteContext()
        try withGrowthWrite(expected: expected) {
            try self.requireEditableMeasurement(entry, in: expected.realm)
            _ = try self.growthBaby(entry.babyID)
            entry.isTombstoned = true
            entry.lastModifiedAt = self.now
            try self.recordLocked(.delete, on: entry)
        }
    }
}

extension BabyStore {
    /// Failed SwiftData saves can leave registered snapshots with attempted
    /// values even after rollback. Refresh the growth rows before returning
    /// the error so held models and a subsequent retry see canonical state.
    /// Baby also needs its edited fields restored from a fresh context, then
    /// rollback to discard that restoration's change tracking. Recovery never
    /// saves: the original operation has failed and must not acquire a write.
    func withGrowthWrite<T>(expected: RealmWriteContext, restoring baby: Baby? = nil,
                            _ body: () throws -> T) throws -> T {
        var enteredWrite = false
        do {
            return try withRealmWrite(expected: expected) {
                enteredWrite = true
                return try body()
            }
        } catch {
            // A rejected permission/generation fence has not run the body or
            // its rollback. Do not touch another pending edit on that path.
            guard enteredWrite else { throw error }
            let key = expected.realm.encodedKey
            func refresh<M: PersistentModel>(_ request: FetchDescriptor<M>) throws {
                var request = request
                request.includePendingChanges = false
                _ = try context.fetch(request)
            }
            try refresh(FetchDescriptor<Baby>(predicate: #Predicate { $0.realmKey == key }))
            try refresh(FetchDescriptor<GrowthProfile>(predicate: #Predicate { $0.realmKey == key }))
            try refresh(FetchDescriptor<MeasurementCheckIn>(predicate: #Predicate { $0.realmKey == key }))
            try refresh(FetchDescriptor<ChangeLogEntry>(predicate: #Predicate { $0.realmKey == key }))
            try refresh(FetchDescriptor<SyncOutboxEntry>(predicate: #Predicate { $0.realmKey == key }))
            if let baby, baby.realmKey == key, baby.modelContext === context {
                let id = baby.id
                let fresh = ModelContext(container)
                if let canonical = try fresh.fetch(FetchDescriptor<Baby>(predicate: #Predicate {
                    $0.id == id && $0.realmKey == key
                })).first {
                    baby.name = canonical.name
                    baby.birthDate = canonical.birthDate
                    baby.birthDateISO = canonical.birthDateISO
                    baby.iconID = canonical.iconID
                    baby.lastModifiedAt = canonical.lastModifiedAt
                    // Keep this after the refetches: refetching while the
                    // restoration is dirty can reinstall the rejected values.
                    context.rollback()
                }
            }
            throw error
        }
    }

    func setGrowthProfileLocked(for babyID: UUID, referenceSex: GrowthReferenceSex, expectedDueDate: LocalDate?) throws {
        if let row = try growthProfile(for: babyID) {
            row.referenceSexRaw = referenceSex.rawValue
            row.expectedDueDateISO = expectedDueDate?.iso8601
            row.lastModifiedAt = now
            try recordLocked(.update, on: row)
        } else {
            let row = GrowthProfile(babyID: babyID, referenceSex: referenceSex, expectedDueDate: expectedDueDate,
                                    modifiedAt: now, realm: activeRealm)
            context.insert(row)
            try recordLocked(.create, on: row)
        }
    }

    func growthBaby(_ id: UUID) throws -> Baby {
        guard let baby = try baby(id), !baby.isTombstoned else { throw GrowthStoreError.babyUnavailable }
        return baby
    }

    private func requireEditableMeasurement(_ entry: MeasurementCheckIn, in realm: RealmID) throws {
        guard entry.realmKey == realm.encodedKey else { throw RealmWriteError.wrongRealm }
        guard entry.modelContext === context, !entry.isTombstoned else { throw GrowthStoreError.measurementUnavailable }
    }

    private func validateMeasurement(for babyID: UUID, at date: Date, timeZoneID: String) throws {
        let baby = try growthBaby(babyID)
        guard date.timeIntervalSince1970.isFinite, baby.birthDate.timeIntervalSince1970.isFinite,
              now.timeIntervalSince1970.isFinite else { throw GrowthStoreError.invalidDate }
        guard let zone = TimeZone(identifier: timeZoneID) else { throw GrowthStoreError.invalidTimeZone }
        guard date <= now else { throw GrowthStoreError.futureMeasurement }
        let measuredDay = try LocalDate(displaying: date, in: zone)
        let birthDay = try baby.localBirthDate ?? LocalDate(displaying: baby.birthDate, in: .current)
        guard measuredDay >= birthDay else { throw GrowthStoreError.measurementBeforeBirth }
    }
}
