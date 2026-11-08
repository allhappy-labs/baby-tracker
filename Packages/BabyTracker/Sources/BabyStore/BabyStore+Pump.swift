import Foundation
import SwiftData
import BabyCore

public enum PumpError: Error, Equatable, Sendable {
    /// A pump is already running in this family.
    case alreadyRunning
    /// End before start, longer than six hours, later than now, or a
    /// finished session asked to run again.
    case invalidTimes
    /// Outside 0–1000 ml.
    case invalidVolume
    /// Stop asked of a session that has already ended.
    case notRunning
}

public extension BabyStore {
    func runningPump() throws -> PumpEntry? {
        let key = activeRealm.encodedKey
        var descriptor = FetchDescriptor<PumpEntry>(predicate: #Predicate {
            $0.realmKey == key && !$0.isTombstoned && $0.endedAt == nil
        })
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .reverse)]
        return try context.fetch(descriptor).first
    }

    func pump(id: UUID) throws -> PumpEntry? {
        let key = activeRealm.encodedKey
        return try context.fetch(FetchDescriptor<PumpEntry>(predicate: #Predicate {
            $0.id == id && $0.realmKey == key && !$0.isTombstoned
        })).first
    }

    func pumps(from start: Date, to end: Date) throws -> [PumpEntry] {
        let key = activeRealm.encodedKey
        var descriptor = FetchDescriptor<PumpEntry>(predicate: #Predicate {
            $0.realmKey == key && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
        })
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .reverse)]
        return try context.fetch(descriptor)
    }

    func lastFinishedPump() throws -> PumpEntry? {
        let key = activeRealm.encodedKey
        var latest = FetchDescriptor<PumpEntry>(predicate: #Predicate {
            $0.realmKey == key && !$0.isTombstoned && $0.endedAt != nil
        }, sortBy: [SortDescriptor(\.endedAt, order: .reverse)])
        latest.fetchLimit = 1
        guard let end = try context.fetch(latest).first?.endedAt else { return nil }
        // Sessions ending at the same instant: the same one on every phone.
        let tied = try context.fetch(FetchDescriptor<PumpEntry>(predicate: #Predicate {
            $0.realmKey == key && !$0.isTombstoned && $0.endedAt == end
        }))
        return tied.max { $0.id.uuidString < $1.id.uuidString }
    }

    func lastPumpVolumeML() throws -> Double? {
        let key = activeRealm.encodedKey
        var latest = FetchDescriptor<PumpEntry>(predicate: #Predicate {
            $0.realmKey == key && !$0.isTombstoned && $0.volumeML != nil
        }, sortBy: [SortDescriptor(\.startedAt, order: .reverse)])
        latest.fetchLimit = 1
        return try context.fetch(latest).first?.volumeML
    }

    /// Starts this family's pumping session. Refused while one runs, on this
    /// phone or (once synced) the other; the server refuses a second one too.
    @discardableResult
    func startPump(side: PumpSide, at date: Date? = nil, expectedWriteContext: RealmWriteContext? = nil) throws -> PumpEntry {
        let expected = try expectedWriteContext ?? captureWriteContext()
        return try withRealmWrite(expected: expected) {
            guard try self.runningPump() == nil else { throw PumpError.alreadyRunning }
            let start = date ?? self.now
            try self.checkPumpTimes(start: start, end: nil)
            let pump = PumpEntry(startedAt: start, timeZoneID: TimeZone.current.identifier, side: side,
                                 createdBy: self.deviceID, realm: self.activeRealm)
            pump.lastModifiedAt = self.now
            self.context.insert(pump)
            try self.recordLocked(.create, on: pump)
            return pump
        }
    }

    /// Ends a running session now (or at `date`). A session forgotten past
    /// six hours ends at six: the longest the server keeps.
    @discardableResult
    func stopPump(_ pump: PumpEntry, at date: Date? = nil, expectedWriteContext: RealmWriteContext? = nil) throws -> Date {
        let expected = try expectedWriteContext ?? captureWriteContext()
        return try withRealmWrite(expected: expected) {
            guard pump.realmKey == expected.realm.encodedKey else { throw RealmWriteError.wrongRealm }
            guard pump.isRunning else { throw PumpError.notRunning }
            // Six hours from the start as the server reads it (whole
            // milliseconds), so the capped end never reads a hair over.
            let sentStart = (try? Date.ISO8601FormatStyle(includingFractionalSeconds: true)
                .parse(MutationFactory.wireTime(pump.startedAt))) ?? pump.startedAt
            let cap = min(pump.startedAt, sentStart).addingTimeInterval(PumpRules.maxSeconds)
            let end = max(pump.startedAt, min(date ?? self.now, cap))
            pump.endedAt = end
            pump.lastModifiedAt = self.now
            try self.recordLocked(.update, on: pump)
            return end
        }
    }

    func setPumpAmount(_ pump: PumpEntry, volumeML: Double?, expectedWriteContext: RealmWriteContext? = nil) throws {
        let expected = try expectedWriteContext ?? captureWriteContext()
        try withRealmWrite(expected: expected) {
            guard pump.realmKey == expected.realm.encodedKey else { throw RealmWriteError.wrongRealm }
            pump.volumeML = try Self.pumpVolume(volumeML)
            pump.lastModifiedAt = self.now
            try self.recordLocked(.update, on: pump)
        }
    }

    @discardableResult
    func logPump(side: PumpSide, from start: Date, to end: Date, volumeML: Double?, note: String?,
                 expectedWriteContext: RealmWriteContext? = nil) throws -> PumpEntry {
        let expected = try expectedWriteContext ?? captureWriteContext()
        return try withRealmWrite(expected: expected) {
            try self.checkPumpTimes(start: start, end: end)
            let amount = try Self.pumpVolume(volumeML)
            let pump = PumpEntry(startedAt: start, endedAt: end, timeZoneID: TimeZone.current.identifier, side: side,
                                 createdBy: self.deviceID, realm: self.activeRealm)
            pump.volumeML = amount
            pump.note = Self.trimmedPumpNote(note)
            pump.lastModifiedAt = self.now
            self.context.insert(pump)
            try self.recordLocked(.create, on: pump)
            return pump
        }
    }

    /// A finished session stays finished: clearing its end is refused, as
    /// the server refuses it (`pump_reopened`). A running one keeps running
    /// when `endedAt` is nil, with its Start and side corrected.
    func updatePump(_ pump: PumpEntry, side: PumpSide, startedAt: Date, endedAt: Date?, volumeML: Double?, note: String?,
                    expectedWriteContext: RealmWriteContext? = nil) throws {
        let expected = try expectedWriteContext ?? captureWriteContext()
        try withRealmWrite(expected: expected) {
            guard pump.realmKey == expected.realm.encodedKey else { throw RealmWriteError.wrongRealm }
            if pump.endedAt != nil, endedAt == nil { throw PumpError.invalidTimes }
            try self.checkPumpTimes(start: startedAt, end: endedAt)
            let amount = try Self.pumpVolume(volumeML)
            pump.side = side
            pump.startedAt = startedAt
            pump.endedAt = endedAt
            pump.volumeML = amount
            pump.note = Self.trimmedPumpNote(note)
            pump.lastModifiedAt = self.now
            try self.recordLocked(.update, on: pump)
        }
    }
}

extension BabyStore {
    /// The server's rules (`validatePumpTimes` plus the record rules' future
    /// check), so nothing is sent only to be refused and quarantined.
    func checkPumpTimes(start: Date, end: Date?) throws {
        let latest = now.addingTimeInterval(PumpRules.clockGrace)
        guard start <= latest else { throw PumpError.invalidTimes }
        guard let end else { return }
        guard end >= start, end <= latest, end.timeIntervalSince(start) <= PumpRules.maxSeconds else {
            throw PumpError.invalidTimes
        }
    }

    static func pumpVolume(_ ml: Double?) throws -> Double? {
        guard let ml else { return nil }
        let rounded = PumpRules.roundedVolume(ml)
        guard PumpRules.isValidVolume(rounded) else { throw PumpError.invalidVolume }
        return rounded
    }

    static func trimmedPumpNote(_ note: String?) -> String? {
        guard let trimmed = note?.trimmingCharacters(in: .whitespacesAndNewlines), !trimmed.isEmpty else { return nil }
        return trimmed
    }
}
